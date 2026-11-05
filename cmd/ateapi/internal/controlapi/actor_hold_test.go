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
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/actorevent"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

func placeTestHold(t *testing.T, ctx context.Context, st store.Interface, actorRef resources.ActorRef) {
	t.Helper()
	if _, _, err := st.PlaceActorHold(ctx, actorRef, func(*ateapi.Actor) error { return nil }, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "hold-1", ActorUid: stored.GetMetadata().GetUid(), Hold: &ateapi.ActorHold{Holder: "user:security", RequestId: "hold-1"}}
	}); err != nil {
		t.Fatalf("PlaceActorHold: %v", err)
	}
}

func placeTestQuarantine(t *testing.T, ctx context.Context, st store.Interface, actorRef resources.ActorRef) {
	t.Helper()
	if _, _, err := st.PlaceActorHold(ctx, actorRef, func(*ateapi.Actor) error { return nil }, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{
			RequestId:  "q-1",
			ActorUid:   stored.GetMetadata().GetUid(),
			Hold:       &ateapi.ActorHold{Holder: store.QuarantineHolder, RequestId: "q-1"},
			Quarantine: &ateapi.ActorQuarantine{RequestId: "q-1", Caller: "user:security"},
		}
	}); err != nil {
		t.Fatalf("PlaceActorHold: %v", err)
	}
}

// A hold placed after the resume read the actor is refused by the write that
// marks it RESUMING, and the worker claim made for that write is released.
func TestEnsureWorkerAssigned_HeldAfterRead(t *testing.T) {
	ctx := context.Background()
	rt := testSandboxRuntime()
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	st := newTestPersistence(t)
	worker := seedRuntimeWorker(t, ctx, st, "amd-pod", "node-amd", rt)
	read := seedActorWithSnapshot(t, ctx, st, ateapi.ActorState_ACTOR_STATE_SUSPENDED, "", durableMemorySnapshot(t, rt))
	placeTestHold(t, ctx, st, actorRef)
	held := mustReadActor(t, ctx, st, actorRef)

	_, _, err := newSchedulingWorkflow(t, ctx, st).ensureWorkerAssigned(ctx, actorRef, read, &ateapi.ActorTemplate{}, nil)
	if got := apierror.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("ensureWorkerAssigned code = %v, want FailedPrecondition (err: %v)", got, err)
	}
	if diff := cmp.Diff(held, mustReadActor(t, ctx, st, actorRef), protocmp.Transform()); diff != "" {
		t.Errorf("refused write changed the actor (-before +after):\n%s", diff)
	}
	assignments, err := st.ListWorkerAssignments(ctx, worker.GetMetadata().GetName(), store.ListOptions{})
	if err != nil {
		t.Fatalf("ListWorkerAssignments: %v", err)
	}
	if len(assignments.Items) != 0 {
		t.Errorf("worker keeps claims %v for a held actor, want none", assignments.Items)
	}
}

// The audit record of a hold names the caller, holder, reason, incident and
// requests, and matches the registry.
func TestLogHoldChanged(t *testing.T) {
	events := otlpEvents(t)
	actor := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: "team-a", Name: "id1", Uid: someActorUID},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "templates", Name: "tmpl"},
		Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}
	hold := &ateapi.ActorHold{Holder: "user:security", Reason: "exfiltration", IncidentId: "INC-7", RequestId: "hold-1"}
	logHoldChanged(context.Background(), actor, ateattr.HoldActionReleased, "user:security", hold, "release-1")

	got := events()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(got), got)
	}
	assertEvent(t, got[0], actorevent.HoldChanged)
	want := map[string]string{
		string(ateattr.AtespaceKey):                  "team-a",
		string(ateattr.ActorNameKey):                 "id1",
		string(ateattr.ActorUIDKey):                  someActorUID,
		string(ateattr.TemplateAtespaceKey):          "templates",
		string(ateattr.TemplateNameKey):              "tmpl",
		string(ateattr.ActorHoldActionKey):           ateattr.HoldActionReleased,
		string(ateattr.ActorHoldHolderKey):           "user:security",
		string(ateattr.ActorHoldCallerKey):           "user:security",
		string(ateattr.ActorHoldReasonKey):           "exfiltration",
		string(ateattr.ActorHoldIncidentIDKey):       "INC-7",
		string(ateattr.ActorHoldRequestIDKey):        "hold-1",
		string(ateattr.ActorHoldReleaseRequestIDKey): "release-1",
		string(ateattr.ActorStateKey):                ateattr.ActorStateSuspended,
	}
	if diff := cmp.Diff(want, got[0].attrs); diff != "" {
		t.Errorf("attributes (-want +got):\n%s", diff)
	}
}
