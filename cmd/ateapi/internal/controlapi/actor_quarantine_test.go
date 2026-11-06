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
	"go.ate.dev/substrate/internal/actorevent"
	"go.ate.dev/substrate/internal/ateattr"
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
