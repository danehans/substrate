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
