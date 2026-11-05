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
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

// The write that marks the actor RESUMING checks the call's conditions
// itself. A retried assignment attempt works from a refreshed actor that the
// call did not check under the lease, so the write must refuse it.
func TestAssignWorkerAttempt_ChecksConditionsInWrite(t *testing.T) {
	rt := testSandboxRuntime()
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	for _, tc := range []struct {
		name string
		cond func(actor *ateapi.Actor) lifecycleConditions
		want codes.Code
	}{
		{"matching uid", func(a *ateapi.Actor) lifecycleConditions {
			return lifecycleConditions{expectedUID: a.GetMetadata().GetUid(), requestID: "r1"}
		}, codes.OK},
		{"other uid", func(*ateapi.Actor) lifecycleConditions {
			return lifecycleConditions{expectedUID: someActorUID, requestID: "r1"}
		}, codes.FailedPrecondition},
		{"an activation the suspended actor does not hold", func(*ateapi.Actor) lifecycleConditions {
			return lifecycleConditions{expectedActivation: "01JDQ5W2R3V6Z8B4N7K9M1P0XA"}
		}, codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newTestPersistence(t)
			seedRuntimeWorker(t, ctx, st, "amd-pod", "node-amd", rt)
			actor := seedActorWithSnapshot(t, ctx, st, ateapi.ActorState_ACTOR_STATE_SUSPENDED, "", durableMemorySnapshot(t, rt))
			call := newLifecycleCall(actorRef, tc.cond(actor), ateapi.LifecycleOperation_LIFECYCLE_OPERATION_RESUME, ateapi.DurabilityMode_DURABILITY_MODE_UNSPECIFIED)

			stored, _, err := newSchedulingWorkflow(t, ctx, st).assignWorkerAttempt(ctx, actorRef, actor, &ateapi.ActorTemplate{}, call)
			if got := apierror.Code(err); got != tc.want {
				t.Fatalf("assignWorkerAttempt code = %v, want %v (err: %v)", got, tc.want, err)
			}
			if tc.want == codes.OK {
				if got := stored.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_RESUMING {
					t.Errorf("state = %v, want RESUMING", got)
				}
				return
			}
			if diff := cmp.Diff(actor, mustReadActor(t, ctx, st, actorRef), protocmp.Transform()); diff != "" {
				t.Errorf("refused write changed the actor (-before +after):\n%s", diff)
			}
		})
	}
}
