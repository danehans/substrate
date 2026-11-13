// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/utils/clock"

	"go.ate.dev/substrate/cmd/atelet/internal/ateletpath"
	"go.ate.dev/substrate/cmd/atelet/internal/snapshottracker"
	"go.ate.dev/substrate/internal/proto/ateletpb"
)

// Held cleanup must leave the original checkpoint restorable, including when
// the upload has not started. A later deletion still reclaims the node state.
func TestTerminatePreservesSnapshots(t *testing.T) {
	for _, mode := range []ateletpb.UploadMode{
		ateletpb.UploadMode_UPLOAD_MODE_DEFERRED,
		ateletpb.UploadMode_UPLOAD_MODE_IMMEDIATE,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			a := runPluginActor(t)
			snap := a.snapshot("held-snapshot", 1)
			if _, err := a.checkpoint(t, snap, mode); err != nil {
				t.Fatal(err)
			}
			a.restoreLocal(t, "held-snapshot", 1)
			before := a.s.snapshots.State(pluginActorUID)
			req := heldTerminateRequest(a)
			// A teardown failure must retain both the workload's directories and
			// its snapshot so a matching retry can finish.
			a.worker.onTerminate = func() error { return status.Error(codes.Internal, "teardown failed") }
			if _, err := a.s.Terminate(t.Context(), req); err == nil {
				t.Fatal("Terminate ignored the worker failure")
			}
			a.worker.onTerminate = nil
			for range 2 {
				transient := filepath.Join(ateletpath.RestoreStateDir(pluginActorUID), "leftover")
				if err := os.WriteFile(transient, []byte("transient"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := a.s.Terminate(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(transient); !os.IsNotExist(err) {
					t.Fatalf("transient restore file survived cleanup: %v", err)
				}
				if after := a.s.snapshots.State(pluginActorUID); !proto.Equal(before, after) {
					t.Fatalf("snapshot state changed: before %v, after %v", before, after)
				}
			}
			if got := a.worker.terminations.Load(); got != 3 {
				t.Fatalf("worker termination calls = %d, want 3", got)
			}
			a.restoreLocal(t, "held-snapshot", 1)
			if got := a.worker.restored["checkpoint.img"]; got != "guest-memory" {
				t.Fatalf("restored memory = %q", got)
			}
			req.PreserveSnapshots = false
			if _, err := a.s.Terminate(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			assertDeletedSnapshotState(t, a)
		})
	}
}

// Uploads can finish during or after held cleanup. Ordinary deletion must
// still cancel and join an upload before removing the files it reads.
func TestHeldCleanupDuringUpload(t *testing.T) {
	for _, finish := range []string{"during cleanup", "after cleanup", "deleted while pending"} {
		t.Run(finish, func(t *testing.T) {
			a := runPluginActor(t)
			store := &gatingTrackerStore{
				inner: &trackerStore{s: a.s}, started: make(chan struct{}, 1),
				release: make(chan struct{}), canceled: make(chan struct{}, 1),
				uploaded: make(chan struct{}, 1),
			}
			reporter := newFakeReportService()
			a.s.snapshots = snapshottracker.New(t.Context(), store, clock.RealClock{},
				snapshottracker.WithUploadDelay(0),
				snapshottracker.WithReporter(&trackerReporter{workers: reporter}))
			a.s.snapshots.Start()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := a.s.snapshots.Wait(ctx); err != nil {
					t.Error(err)
				}
			})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(store.release) }) }
			t.Cleanup(release)
			snap := a.snapshot("held-snapshot", 1)
			if _, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED); err != nil {
				t.Fatal(err)
			}
			select {
			case <-store.started:
			case <-time.After(10 * time.Second):
				t.Fatal("background upload did not start")
			}
			a.restoreLocal(t, "held-snapshot", 1)
			if finish == "during cleanup" {
				a.worker.onTerminate = func() error {
					release()
					select {
					case <-store.uploaded:
						return nil
					case <-time.After(10 * time.Second):
						return status.Error(codes.DeadlineExceeded, "upload did not complete during teardown")
					}
				}
			}
			req := heldTerminateRequest(a)
			stopped := make(chan error, 1)
			go func() { _, err := a.s.Terminate(t.Context(), req); stopped <- err }()
			select {
			case <-store.canceled:
				release()
				<-stopped
				t.Fatal("held cleanup canceled the background upload")
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				release()
				<-stopped
				t.Fatal("held cleanup waited for the pending upload")
			}
			a.worker.onTerminate = nil
			a.restoreLocal(t, "held-snapshot", 1)
			if finish == "deleted while pending" {
				req.PreserveSnapshots = false
				done := make(chan error, 1)
				go func() { _, err := a.s.Terminate(t.Context(), req); done <- err }()
				select {
				case <-store.canceled:
				case <-time.After(10 * time.Second):
					t.Fatal("deletion did not cancel the pending upload")
				}
				if _, err := os.Stat(ateletpath.LocalSnapshotDir(pluginActorUID, "held-snapshot")); err != nil {
					t.Fatalf("deletion removed files before the upload stopped: %v", err)
				}
				release()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else {
				release()
				select {
				case report := <-reporter.reports:
					if report.GetState().GetLatestDurable().GetUuid() != snap.GetSnapshotUuid() {
						t.Fatalf("upload report lost the snapshot: %v", report)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("preserved upload did not become durable")
				}
				a.restoreLocal(t, "held-snapshot", 1)
				req.PreserveSnapshots = false
				if _, err := a.s.Terminate(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			assertDeletedSnapshotState(t, a)
		})
	}
}

func heldTerminateRequest(a *pluginActor) *ateletpb.TerminateRequest {
	return &ateletpb.TerminateRequest{
		Atespace: "ate-demo", ActorName: "counter", ActorUid: pluginActorUID,
		WorkerPodUid: pluginWorkerPodUID, Spec: a.spec, PreserveSnapshots: true,
	}
}

func assertDeletedSnapshotState(t *testing.T, a *pluginActor) {
	t.Helper()
	if st := a.s.snapshots.State(pluginActorUID); !proto.Equal(st, &ateletpb.ActorSnapshotState{}) {
		t.Fatalf("snapshot state survived deletion: %v", st)
	}
	for _, path := range []string{ateletpath.ActorPath(pluginActorUID), ateletpath.ActorSnapshotPath(pluginActorUID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived deletion: %v", path, err)
		}
	}
}
