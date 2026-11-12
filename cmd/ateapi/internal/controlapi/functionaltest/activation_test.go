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

package functionaltest

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.ate.dev/substrate/api/ateapi/v1"
)

// sentActivation returns the activation of the Run or Restore the fake atelet
// received since the last call, and clears both.
func sentActivation(t *testing.T, tc *testContext) string {
	t.Helper()
	f := tc.fakeAtelet
	f.Lock.Lock()
	defer f.Lock.Unlock()
	run, restore := f.RunRequest, f.RestoreRequest
	f.RunRequest, f.RestoreRequest = nil, nil
	switch {
	case run != nil && restore != nil:
		t.Fatalf("atelet received both a Run and a Restore")
	case run != nil:
		return run.GetActivationId()
	case restore != nil:
		return restore.GetActivationId()
	}
	t.Fatalf("atelet received no Run or Restore")
	return ""
}

func mustParseActivation(t *testing.T, id string) {
	t.Helper()
	if _, err := ulid.ParseStrict(id); err != nil {
		t.Fatalf("activation %q is not a ULID: %v", id, err)
	}
}

// Each resume mints an activation, sends it on Run or Restore and keeps it
// on the RUNNING Actor. Suspend clears it with the worker assignment, and the
// next resume mints another.
func TestActivation_ResumeSuspendResume(t *testing.T) {
	ns := namespaceForTest("ns-activation")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	ctx := context.Background()

	createTemplate(t, tc, ns)
	createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	ref := &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}
	created, err := tc.client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	if got := created.GetStatus().GetActivationId(); got != "" {
		t.Fatalf("new actor activation_id = %q, want none", got)
	}

	resume := func() string {
		t.Helper()
		resp, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
		if err != nil {
			t.Fatalf("ResumeActor: %v", err)
		}
		got := resp.GetActor().GetStatus().GetActivationId()
		mustParseActivation(t, got)
		if sent := sentActivation(t, tc); sent != got {
			t.Errorf("atelet was sent activation %q, actor holds %q", sent, got)
		}
		return got
	}

	first := resume()
	if _, err := tc.client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	suspended, err := tc.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got := suspended.GetStatus().GetActivationId(); got != "" {
		t.Errorf("suspended actor activation_id = %q, want it cleared with the worker assignment", got)
	}
	if second := resume(); second == first {
		t.Errorf("second resume reused activation %q", first)
	}
}

// A resume whose Run or Restore fails after the RESUMING write leaves the Actor RESUMING.
// The retry restores under a new activation, so the failed one is never sent
// again.
func TestActivation_RetryAfterFailedRunRotates(t *testing.T) {
	ns := namespaceForTest("ns-activation-retry")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	ctx := context.Background()

	createTemplate(t, tc, ns)
	createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	ref := &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}
	if _, err := tc.client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}}); err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}

	tc.fakeAtelet.Lock.Lock()
	tc.fakeAtelet.FailRun = status.Error(codes.Unavailable, "worker unreachable")
	tc.fakeAtelet.FailRestore = tc.fakeAtelet.FailRun
	tc.fakeAtelet.Lock.Unlock()
	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); status.Code(err) != codes.Unavailable {
		t.Fatalf("first ResumeActor = %v, want Unavailable", err)
	}
	failed := sentActivation(t, tc)
	mustParseActivation(t, failed)
	stuck, err := tc.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got := stuck.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_RESUMING {
		t.Fatalf("state after the failed call = %v, want RESUMING", got)
	}
	if got := stuck.GetStatus().GetActivationId(); got != failed {
		t.Fatalf("RESUMING actor activation_id = %q, want %q, the one sent to atelet", got, failed)
	}

	tc.fakeAtelet.Lock.Lock()
	tc.fakeAtelet.FailRun, tc.fakeAtelet.FailRestore = nil, nil
	tc.fakeAtelet.Lock.Unlock()
	resp, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("retried ResumeActor: %v", err)
	}
	retried := sentActivation(t, tc)
	mustParseActivation(t, retried)
	if retried == failed {
		t.Fatalf("retry sent the failed attempt's activation %q again", failed)
	}
	if got := resp.GetActor().GetStatus().GetActivationId(); got != retried {
		t.Errorf("RUNNING actor activation_id = %q, want %q, the one sent on the retry", got, retried)
	}
}

// A Run or Restore that crashes the Actor records the crashed activation on ActorCrash
// and clears it from the status with the worker assignment.
func TestActivation_CrashRecordsActivation(t *testing.T) {
	ns := namespaceForTest("ns-activation-crash")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	ctx := context.Background()

	createTemplate(t, tc, ns)
	createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	ref := &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}
	if _, err := tc.client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}}); err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}

	tc.fakeAtelet.Lock.Lock()
	tc.fakeAtelet.FailRun = status.Error(codes.InvalidArgument, "bad workload")
	tc.fakeAtelet.FailRestore = tc.fakeAtelet.FailRun
	tc.fakeAtelet.Lock.Unlock()
	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); err == nil {
		t.Fatal("ResumeActor succeeded, want the failed call to crash the actor")
	}
	sent := sentActivation(t, tc)
	mustParseActivation(t, sent)

	crashed, err := tc.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got := crashed.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("state = %v, want CRASHED", got)
	}
	if got := crashed.GetStatus().GetCrash().GetActivationId(); got != sent {
		t.Errorf("crash activation_id = %q, want %q, the one sent to atelet", got, sent)
	}
	if got := crashed.GetStatus().GetActivationId(); got != "" {
		t.Errorf("crashed actor activation_id = %q, want it cleared with the worker assignment", got)
	}
}
