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

// A hold that outlived its receipt still has one after the release, so a
// late retry of the hold finds it and does not hold the Actor again.
func TestReleaseActorHold_KeepsHoldReceipt(t *testing.T) {
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
	allow := func(*ateapi.Actor) error { return nil }
	if _, _, err := s.PlaceActorHold(ctx, actorRef, allow, func(*ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "hold-1", Hold: &ateapi.ActorHold{Holder: "user:security", RequestId: "hold-1"}}
	}); err != nil {
		t.Fatalf("PlaceActorHold failed: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE actor_lifecycle_receipts SET expire_time = $1 WHERE request_id = 'hold-1'`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("expiring the hold receipt: %v", err)
	}
	if n, err := s.cleanupExpiredLifecycleReceipts(ctx); err != nil || n != 1 {
		t.Fatalf("cleanupExpiredLifecycleReceipts = %d, %v, want 1", n, err)
	}

	// Expiry must not let another operation claim the active hold's key.
	held, err := s.GetActor(ctx, actorRef)
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.GetActorHold(ctx, actorRef, "user:security")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateActorWithReceipt(ctx, actorRef, store.PreconditionFrom(held), nil, func(*ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "hold-1", Operation: ateapi.LifecycleOperation_LIFECYCLE_OPERATION_SUSPEND}
	}, true); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("reused active hold key = %v, want ErrAlreadyExists", err)
	}
	after, err := s.GetActorLifecycleReceipt(ctx, actorRef, "hold-1")
	if err != nil || !proto.Equal(original, after) {
		t.Fatalf("hold receipt changed: %v, %v", after, err)
	}

	if _, _, err := s.ReleaseActorHold(ctx, actorRef, "user:security", "hold-1", allow, func(*ateapi.Actor, *ateapi.ActorHold) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "release-1"}
	}); err != nil {
		t.Fatalf("ReleaseActorHold failed: %v", err)
	}
	kept, err := s.GetActorLifecycleReceipt(ctx, actorRef, "hold-1")
	if err != nil {
		t.Fatalf("hold receipt after release: %v", err)
	}
	if got := kept.GetHold().GetHolder(); got != "user:security" {
		t.Errorf("kept receipt holder = %q, want user:security", got)
	}
	var expire time.Time
	if err := s.pool.QueryRow(ctx, `SELECT expire_time FROM actor_lifecycle_receipts WHERE request_id = 'hold-1'`).Scan(&expire); err != nil {
		t.Fatal(err)
	}
	if time.Until(expire) < 23*time.Hour {
		t.Errorf("kept receipt row expires at %v, want about a day after the release", expire)
	}
}
