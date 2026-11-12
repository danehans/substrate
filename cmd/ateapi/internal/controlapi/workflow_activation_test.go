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

package controlapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/hardware"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// countingStore counts the Actor writes a workflow makes.
type countingStore struct {
	store.Interface

	mu          sync.Mutex
	actorWrites int
}

func (s *countingStore) UpdateActor(ctx context.Context, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error) (*ateapi.Actor, error) {
	s.mu.Lock()
	s.actorWrites++
	s.mu.Unlock()
	return s.Interface.UpdateActor(ctx, actorRef, precondition, mutate)
}

func (s *countingStore) BindActorToWorker(ctx context.Context, workerName string, assignment *ateapi.ActorAssignment, admit func(*ateapi.Worker) error, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error) (*ateapi.Actor, error) {
	s.mu.Lock()
	s.actorWrites++
	s.mu.Unlock()
	return s.Interface.BindActorToWorker(ctx, workerName, assignment, admit, actorRef, precondition, mutate)
}

func (s *countingStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.actorWrites
}

// TestResumeActor_Activation runs whole resumes from each state a resume can
// find the Actor in and checks the activation it ends with, the activation
// atelet was sent, and how many Actor writes it took.
func TestResumeActor_Activation(t *testing.T) {
	const seeded = "01JDQ5W2R3V6Z8B4N7K9M1P0XA"
	tests := []struct {
		name string
		// state is the state the Actor is seeded in.
		state ateapi.ActorState
		// claimed seeds the worker's claim on the Actor.
		claimed bool
		// writes is the number of Actor writes the resume makes.
		writes int
	}{{
		name:   "first attempt mints in the bind write",
		state:  ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		writes: 2, // the bind, RUNNING
	}, {
		name:    "re-entry replaces the activation before restoring again",
		state:   ateapi.ActorState_ACTOR_STATE_RESUMING,
		claimed: true,
		writes:  2, // the replacement, RUNNING
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)
			newTestActorWorkflow(t, persistence, "ns", "tmpl1")
			workerName := testWorkerUID("pod-1")
			if _, err := persistence.CreateWorker(ctx, &ateapi.Worker{
				Metadata:        &ateapi.ResourceMetadata{Name: workerName},
				WorkerNamespace: "worker-ns",
				WorkerPool:      "pool",
				WorkerPod:       "pod-1",
				WorkerPodUid:    workerName,
				NodeName:        "node-1",
				Status: registered(&ateapi.WorkerStatus{
					State:    ateapi.WorkerState_WORKER_STATE_ACTIVE,
					Capacity: &ateapi.WorkerResources{Actors: 1},
				}),
			}); err != nil {
				t.Fatalf("CreateWorker: %v", err)
			}
			worker, err := persistence.GetWorker(ctx, workerName)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
			seedWorkflowActor(t, ctx, persistence, actorRef, "ns", "tmpl1", tt.state, func(a *ateapi.Actor) {
				if tt.state == ateapi.ActorState_ACTOR_STATE_RESUMING {
					a.Status.WorkerAssignment = workerAssignmentFrom(worker, hardware.Requirement{})
					a.Status.AssignedNode = "node-1"
					a.Status.ActivationId = seeded
				}
			})
			actor, err := persistence.GetActor(ctx, actorRef)
			if err != nil {
				t.Fatalf("GetActor: %v", err)
			}
			if tt.claimed {
				seedAssignment(t, persistence, workerName, &ateapi.ActorAssignment{
					Actor:    &ateapi.ObjectRef{Atespace: actorRef.Atespace, Name: actorRef.Name},
					ActorUid: actor.GetMetadata().GetUid(),
				})
			}

			counting := &countingStore{Interface: persistence}
			w := newSchedulingWorkflow(t, ctx, counting)
			w.workflowDeadline = time.Minute
			fake := &capturingAtelet{}
			w.dialer = newNodeAteletDialer(t, fake)

			got, resumed, _, err := w.ResumeActor(ctx, actorRef, lifecycleConditions{})
			if err != nil || !resumed {
				t.Fatalf("ResumeActor() = resumed %v, err %v", resumed, err)
			}
			if n := counting.writes(); n != tt.writes {
				t.Errorf("resume made %d Actor writes, want %d", n, tt.writes)
			}
			activation := got.GetStatus().GetActivationId()
			if _, err := ulid.ParseStrict(activation); err != nil {
				t.Fatalf("activation_id %q is not a ULID: %v", activation, err)
			}
			if activation == seeded {
				t.Errorf("activation_id = %q, the one the Actor was seeded with", seeded)
			}
			_, run := fake.requests()
			if run.GetActivationId() != activation {
				t.Errorf("Run activation_id = %q, want %q", run.GetActivationId(), activation)
			}
			stored, err := persistence.GetActor(ctx, actorRef)
			if err != nil {
				t.Fatalf("GetActor: %v", err)
			}
			if stored.GetStatus().GetActivationId() != activation {
				t.Errorf("stored activation_id = %q, want %q", stored.GetStatus().GetActivationId(), activation)
			}
		})
	}
}

// Every write that clears the worker assignment clears the activation with
// it, so no activation outlives its assignment.
func TestClearWorkerAssignment(t *testing.T) {
	status := &ateapi.ActorStatus{
		WorkerAssignment: wireTestAssignment(),
		ActivationId:     "01JDQ5W2R3V6Z8B4N7K9M1P0XA",
		AssignedNode:     "node-1",
	}
	clearWorkerAssignment(status)
	if status.GetWorkerAssignment() != nil || status.GetActivationId() != "" {
		t.Errorf("after clearWorkerAssignment: assignment %v, activation %q, want both unset", status.GetWorkerAssignment(), status.GetActivationId())
	}
	if status.GetAssignedNode() != "node-1" {
		t.Errorf("assigned_node = %q, want it kept", status.GetAssignedNode())
	}
}

func TestLifecycleCompletion_RejectsLaterExecution(t *testing.T) {
	for _, change := range []string{"activation", "uid"} {
		t.Run(change, func(t *testing.T) {
			st := newTestPersistence(t)
			ctx := t.Context()
			ref := resources.ActorRef{Atespace: "team-a", Name: "id1"}
			seedActor(t, ctx, st, ref)
			actor := loadCrashActor(t, st, ref)
			actor, err := st.UpdateActor(ctx, ref, store.PreconditionFrom(actor), func(a *ateapi.Actor) error {
				a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDING
				a.Status.ActivationId = "01JDQ5W2R3V6Z8B4N7K9M1P0XA"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			stale := proto.CloneOf(actor)
			if change == "uid" {
				stale.Metadata.Uid = "earlier-uid"
			} else {
				stale.Status.ActivationId = "01JDQ5W2R3V6Z8B4N7K9M1P0XB"
			}
			w, _ := newFinalizeWorkflow(st)
			if _, _, err := w.ensureSuspendedFinalized(ctx, stale, suspendSync, nil, nil); apierror.Code(err) != codes.Aborted {
				t.Fatalf("stale suspend completion = %v, want Aborted", err)
			}
			if err := crashActorIfCurrent(ctx, st, stale, "suspend", "stale failure"); apierror.Code(err) != codes.Aborted {
				t.Fatalf("stale crash = %v, want Aborted", err)
			}
			if after := loadCrashActor(t, st, ref); !proto.Equal(actor, after) {
				t.Fatalf("later execution changed: %v", after)
			}
		})
	}
}
