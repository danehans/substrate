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

package atepg

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
)

// An expired receipt is not returned, a new call may store a receipt under its
// key, and cleanup deletes only expired rows.
func TestLifecycleReceipt_Expiry(t *testing.T) {
	s := setupPostgresPersistence(t)
	ctx := context.Background()
	createTestAtespace(t, s, "team-a")
	created, err := s.CreateActor(ctx, &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: "team-a", Name: "session-1"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "team-a", Name: "tmpl"},
		Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	actorRef := resources.ActorRefFromActor(created)
	receipt := func(id string) func(*ateapi.Actor) *ateapi.LifecycleReceipt {
		return func(*ateapi.Actor) *ateapi.LifecycleReceipt {
			return &ateapi.LifecycleReceipt{RequestId: id, State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}
		}
	}
	for _, id := range []string{"expired", "live"} {
		if _, _, err := s.UpdateActorWithReceipt(ctx, actorRef, store.PreconditionFrom(created), nil, receipt(id), true); err != nil {
			t.Fatalf("UpdateActorWithReceipt(%s) failed: %v", id, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE actor_lifecycle_receipts SET expire_time = $1 WHERE request_id = 'expired'`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("expiring a receipt: %v", err)
	}

	if _, err := s.GetActorLifecycleReceipt(ctx, actorRef, "expired"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetActorLifecycleReceipt of an expired receipt = %v, want ErrNotFound", err)
	}
	if _, _, err := s.UpdateActorWithReceipt(ctx, actorRef, store.PreconditionFrom(created), nil, receipt("expired"), false); !errors.Is(err, store.ErrVersionConflict) {
		t.Errorf("replacing an expired receipt = %v, want ErrVersionConflict", err)
	}

	// A new call may store a receipt over the expired one.
	if _, _, err := s.UpdateActorWithReceipt(ctx, actorRef, store.PreconditionFrom(created), nil, receipt("expired"), true); err != nil {
		t.Fatalf("storing a receipt under an expired key: %v", err)
	}
	if _, err := s.GetActorLifecycleReceipt(ctx, actorRef, "expired"); err != nil {
		t.Errorf("GetActorLifecycleReceipt of the stored-over receipt: %v", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE actor_lifecycle_receipts SET expire_time = $1 WHERE request_id = 'expired'`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("expiring a receipt: %v", err)
	}
	deleted, err := s.cleanupExpiredLifecycleReceipts(ctx)
	if err != nil {
		t.Fatalf("cleanupExpiredLifecycleReceipts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if _, err := s.GetActorLifecycleReceipt(ctx, actorRef, "live"); err != nil {
		t.Errorf("GetActorLifecycleReceipt of the live receipt after cleanup: %v", err)
	}
}

func TestLifecycleReceipt_Restart(t *testing.T) {
	pool := requirePool(t)
	ctx := t.Context()
	const schema = "lifecycle-restart"
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS "lifecycle-restart" CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA "lifecycle-restart" CASCADE`) })
	p, err := Connect(ctx, testConnectConfig(schema))
	if err != nil {
		t.Fatal(err)
	}
	createTestAtespace(t, p, "team-a")
	actor, err := p.CreateActor(ctx, &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: "team-a", Name: "actor"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "team-a", Name: "template"},
		Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := resources.ActorRefFromActor(actor)
	build := func(a *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "suspend-1", ActorUid: a.GetMetadata().GetUid(),
			Operation: ateapi.LifecycleOperation_LIFECYCLE_OPERATION_SUSPEND,
			State:     ateapi.ActorState_ACTOR_STATE_SUSPENDED, ActivationId: "earlier-run", SnapshotDurable: false}
	}
	_, receipt, err := p.UpdateActorWithReceipt(ctx, ref, store.PreconditionFrom(actor), nil, build, true)
	if err != nil {
		t.Fatal(err)
	}
	_, hold, err := p.PlaceActorHold(ctx, ref, func(*ateapi.Actor) error { return nil }, func(a *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "hold-1", ActorUid: a.GetMetadata().GetUid(), Hold: &ateapi.ActorHold{Holder: "user:security", RequestId: "hold-1"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.pool.Close()
	p, err = Connect(ctx, testConnectConfig(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer p.pool.Close()
	defer p.Close()
	got, err := p.GetActorLifecycleReceipt(ctx, ref, "suspend-1")
	if err != nil || !proto.Equal(receipt, got) {
		t.Fatalf("reopened receipt = %v, %v", got, err)
	}
	gotHold, err := p.GetActorHold(ctx, ref, "user:security")
	if err != nil || !proto.Equal(hold, gotHold) {
		t.Fatalf("reopened hold = %v, %v", gotHold, err)
	}
	current, err := p.GetActor(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	// A retry cannot replace the durable receipt with later state.
	if _, _, err := p.UpdateActorWithReceipt(ctx, ref, store.PreconditionFrom(current), func(a *ateapi.Actor) error {
		a.Status.State = ateapi.ActorState_ACTOR_STATE_RESUMING
		a.Status.ActivationId = "later-run"
		return nil
	}, build, true); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("retry after restart = %v", err)
	}
	after, err := p.GetActor(ctx, ref)
	if err != nil || !proto.Equal(current, after) {
		t.Fatalf("failed retry changed actor: %v, %v", after, err)
	}
}

func TestLifecycleReceipt_AssignmentRollback(t *testing.T) {
	for _, failure := range []string{"admission", "bind receipt", "release receipt"} {
		t.Run(failure, func(t *testing.T) {
			p := setupPostgresPersistence(t)
			ctx := t.Context()
			createTestAtespace(t, p, "team-a")
			actor, err := p.CreateActor(ctx, &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: "team-a", Name: "actor"}, Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}})
			if err != nil {
				t.Fatal(err)
			}
			ref := resources.ActorRefFromActor(actor)
			worker, err := p.CreateWorker(ctx, &ateapi.Worker{Metadata: &ateapi.ResourceMetadata{Name: "worker"}})
			if err != nil {
				t.Fatal(err)
			}
			receipt := func(a *ateapi.Actor) *ateapi.LifecycleReceipt {
				return &ateapi.LifecycleReceipt{RequestId: "request-1", ActorUid: a.GetMetadata().GetUid(), State: a.GetStatus().GetState(), ActivationId: a.GetStatus().GetActivationId()}
			}
			var original *ateapi.LifecycleReceipt
			if failure != "admission" {
				_, original, err = p.UpdateActorWithReceipt(ctx, ref, store.PreconditionFrom(actor), nil, receipt, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			assignment := &ateapi.ActorAssignment{Actor: &ateapi.ObjectRef{Atespace: ref.Atespace, Name: ref.Name}, ActorUid: actor.GetMetadata().GetUid()}
			bind := func(a *ateapi.Actor) error {
				a.Status.State = ateapi.ActorState_ACTOR_STATE_RESUMING
				a.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: "worker"}}
				a.Status.ActivationId = "attempt-1"
				return nil
			}
			if failure == "release receipt" {
				actor, err = p.BindActorToWorker(ctx, "worker", assignment, nil, ref, store.PreconditionFrom(actor), bind)
				if err != nil {
					t.Fatal(err)
				}
				worker, err = p.GetWorker(ctx, "worker")
				if err != nil {
					t.Fatal(err)
				}
				_, released, _, err := p.UpdateActorAndReleaseWithReceipt(ctx, ref, store.PreconditionFrom(actor), func(a *ateapi.Actor) error {
					a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
					a.Status.WorkerAssignment = nil
					a.Status.ActivationId = ""
					return nil
				}, receipt, true)
				if !errors.Is(err, store.ErrAlreadyExists) || released != nil {
					t.Fatalf("conflicting release returned %v, %v", released, err)
				}
			} else {
				var admit func(*ateapi.Worker) error
				wantErr := store.ErrAlreadyExists
				if failure == "admission" {
					wantErr = errors.New("worker full")
					admit = func(*ateapi.Worker) error { return wantErr }
				}
				if _, _, err := p.BindActorToWorkerWithReceipt(ctx, "worker", assignment, admit, ref, store.PreconditionFrom(actor), bind, receipt, true); !errors.Is(err, wantErr) {
					t.Fatalf("failed bind = %v, want %v", err, wantErr)
				}
			}
			gotActor, err := p.GetActor(ctx, ref)
			if err != nil || !proto.Equal(actor, gotActor) {
				t.Fatalf("actor changed on failure: %v, %v", gotActor, err)
			}
			gotWorker, err := p.GetWorker(ctx, "worker")
			if err != nil || !proto.Equal(worker, gotWorker) {
				t.Fatalf("worker changed on failure: %v, %v", gotWorker, err)
			}
			gotReceipt, err := p.GetActorLifecycleReceipt(ctx, ref, "request-1")
			if original == nil {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("failed claim stored receipt: %v, %v", gotReceipt, err)
				}
			} else if err != nil || !proto.Equal(original, gotReceipt) {
				t.Fatalf("receipt changed on failure: %v, %v", gotReceipt, err)
			}
			_, err = p.GetWorkerAssignment(ctx, "worker", actor.GetMetadata().GetUid())
			if failure == "release receipt" && err != nil || failure != "release receipt" && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unexpected assignment after failure: %v", err)
			}
		})
	}
}
