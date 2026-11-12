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
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
)

// The audit record of a quarantine names the caller, reason, incident,
// request, the activation it found and the steps in effect, and matches the
// registry. A release names the activation the quarantine found, which the
// suspend has cleared from the Actor.
func TestLogQuarantineChanged(t *testing.T) {
	actor := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: "team-a", Name: "id1", Uid: someActorUID},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "templates", Name: "tmpl"},
		Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}
	quarantine := &ateapi.LifecycleReceipt{
		RequestId:    "q-1",
		ActivationId: "01JDQ5W2R3V6Z8B4N7K9M1P0XA",
		Caller:       "user:detector",
		Reason:       "exfiltration",
		Hold:         &ateapi.ActorHold{Holder: "quarantine", RequestId: "q-1", IncidentId: "INC-7"},
		Steps: []ateapi.QuarantineStep{
			ateapi.QuarantineStep_QUARANTINE_STEP_HOLD,
			ateapi.QuarantineStep_QUARANTINE_STEP_MARKED,
			ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED,
		},
	}
	release := &ateapi.LifecycleReceipt{
		RequestId:  "release-1",
		Caller:     "user:admin",
		Reason:     "cleared",
		Hold:       &ateapi.ActorHold{Holder: "quarantine", RequestId: "q-1", IncidentId: "INC-7"},
		Quarantine: &ateapi.ActorQuarantine{RequestId: "q-1", ActivationId: "01JDQ5W2R3V6Z8B4N7K9M1P0XA"},
	}
	identity := map[string]string{
		string(ateattr.AtespaceKey):                  "team-a",
		string(ateattr.ActorNameKey):                 "id1",
		string(ateattr.ActorUIDKey):                  someActorUID,
		string(ateattr.TemplateAtespaceKey):          "templates",
		string(ateattr.TemplateNameKey):              "tmpl",
		string(ateattr.ActorActivationKey):           "01JDQ5W2R3V6Z8B4N7K9M1P0XA",
		string(ateattr.ActorStateKey):                ateattr.ActorStateSuspended,
		string(ateattr.ActorQuarantineRequestIDKey):  "q-1",
		string(ateattr.ActorQuarantineIncidentIDKey): "INC-7",
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range identity {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	tests := []struct {
		name             string
		action           string
		r                *ateapi.LifecycleReceipt
		releaseRequestID string
		want             map[string]string
	}{{
		name:   "suspended",
		action: ateattr.QuarantineActionSuspended,
		r:      quarantine,
		want: with(map[string]string{
			string(ateattr.ActorQuarantineActionKey): ateattr.QuarantineActionSuspended,
			string(ateattr.ActorQuarantineCallerKey): "user:detector",
			string(ateattr.ActorQuarantineReasonKey): "exfiltration",
			string(ateattr.ActorQuarantineStepsKey):  "hold,marked,suspended",
		}),
	}, {
		name:   "terminated",
		action: ateattr.QuarantineActionTerminated,
		r: func() *ateapi.LifecycleReceipt {
			r := ateapi.LifecycleReceipt{
				RequestId: quarantine.GetRequestId(), ActivationId: quarantine.GetActivationId(),
				Caller: quarantine.GetCaller(), Reason: quarantine.GetReason(), Hold: quarantine.GetHold(),
				Steps: []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED, ateapi.QuarantineStep_QUARANTINE_STEP_TERMINATED},
			}
			return &r
		}(),
		want: with(map[string]string{
			string(ateattr.ActorQuarantineActionKey): ateattr.QuarantineActionTerminated,
			string(ateattr.ActorQuarantineCallerKey): "user:detector",
			string(ateattr.ActorQuarantineReasonKey): "exfiltration",
			string(ateattr.ActorQuarantineStepsKey):  "hold,marked,terminated",
		}),
	}, {
		name:             "released",
		action:           ateattr.QuarantineActionReleased,
		r:                release,
		releaseRequestID: "release-1",
		want: with(map[string]string{
			string(ateattr.ActorQuarantineActionKey):           ateattr.QuarantineActionReleased,
			string(ateattr.ActorQuarantineCallerKey):           "user:admin",
			string(ateattr.ActorQuarantineReasonKey):           "cleared",
			string(ateattr.ActorQuarantineReleaseRequestIDKey): "release-1",
		}),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := otlpEvents(t)
			logQuarantineChanged(context.Background(), actor, tt.action, tt.r, tt.releaseRequestID)
			got := events()
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1: %v", len(got), got)
			}
			assertEvent(t, got[0], actorevent.QuarantineChanged)
			if diff := cmp.Diff(tt.want, got[0].attrs); diff != "" {
				t.Errorf("attributes (-want +got):\n%s", diff)
			}
		})
	}

	// A refused resume names the quarantine it hit.
	t.Run("resume refused", func(t *testing.T) {
		events := otlpEvents(t)
		refused := &ateapi.Actor{Metadata: actor.Metadata, ActorTemplate: actor.ActorTemplate, Status: &ateapi.ActorStatus{
			State: ateapi.ActorState_ACTOR_STATE_SUSPENDED,
			Quarantine: &ateapi.ActorQuarantine{
				RequestId: "q-1", Reason: "exfiltration", IncidentId: "INC-7", ActivationId: "01JDQ5W2R3V6Z8B4N7K9M1P0XA",
			},
		}}
		logResumeRefusedByQuarantine(context.Background(), refused)
		got := events()
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1: %v", len(got), got)
		}
		assertEvent(t, got[0], actorevent.QuarantineChanged)
		want := with(map[string]string{
			string(ateattr.ActorQuarantineActionKey): ateattr.QuarantineActionResumeRefused,
			string(ateattr.ActorQuarantineReasonKey): "exfiltration",
		})
		if diff := cmp.Diff(want, got[0].attrs); diff != "" {
			t.Errorf("attributes (-want +got):\n%s", diff)
		}
	})
}

// A duplicate read the initial receipt before another call finished fallback
// termination. Its delayed suspend completion must preserve that evidence.
func TestQuarantineDelayedSuspendKeepsTerminationReceipt(t *testing.T) {
	st := newTestPersistence(t)
	w := newTestActorWorkflow(t, st, "team-a", "template")
	ctx := t.Context()
	ref := resources.ActorRef{Atespace: "team-a", Name: "actor"}
	seedWorkflowActor(t, ctx, st, ref, "team-a", "template", ateapi.ActorState_ACTOR_STATE_RUNNING, func(a *ateapi.Actor) {
		a.Status.ActivationId = testActivationID
	})
	service := &RPCService{impl: &ServiceImpl{store: st}, actorWorkflow: w}
	actor, initial, err := st.PlaceActorHold(ctx, ref, func(*ateapi.Actor) error { return nil }, func(a *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{
			RequestId: "quarantine-1", SuspendRequestId: "quarantine-1/suspend",
			ActorUid: a.GetMetadata().GetUid(), ActivationId: testActivationID,
			Operation:  ateapi.LifecycleOperation_LIFECYCLE_OPERATION_QUARANTINE,
			State:      ateapi.ActorState_ACTOR_STATE_RUNNING,
			Hold:       &ateapi.ActorHold{Holder: store.QuarantineHolder, RequestId: "quarantine-1"},
			Quarantine: &ateapi.ActorQuarantine{RequestId: "quarantine-1", ActivationId: testActivationID},
			Steps:      []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpdateActorAndRelease(ctx, ref, store.PreconditionFrom(actor), func(a *ateapi.Actor) error {
		a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
		clearWorkerAssignment(a.Status)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	terminal := proto.CloneOf(initial)
	terminal.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
	terminal.Steps = append(terminal.Steps, ateapi.QuarantineStep_QUARANTINE_STEP_TERMINATED)
	if err := st.ReplaceActorHoldReceipt(ctx, ref, terminal); err != nil {
		t.Fatal(err)
	}
	_, got, err := service.finishQuarantineSuspend(ctx, ref, initial)
	if err != nil || !proto.Equal(terminal, got) {
		t.Fatalf("delayed completion = %v, %v; want %v", got, err, terminal)
	}
	stored, err := st.GetActorLifecycleReceipt(ctx, ref, initial.GetRequestId())
	if err != nil || !proto.Equal(terminal, stored) {
		t.Fatalf("stored receipt = %v, %v; want %v", stored, err, terminal)
	}
}
