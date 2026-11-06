// Copyright 2026 Google LLC
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
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (p *Persistence) GetActorHold(ctx context.Context, actorRef resources.ActorRef, holder string) (*ateapi.LifecycleReceipt, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `
		SELECT receipt FROM actor_holds
		WHERE atespace = $1 AND actor_name = $2 AND holder = $3`,
		actorRef.Atespace, actorRef.Name, holder).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting hold of %q on actor %s: %w", holder, actorRef, err)
	}
	return unmarshalHoldReceipt(b, actorRef)
}

func (p *Persistence) PlaceActorHold(ctx context.Context, actorRef resources.ActorRef, check func(*ateapi.Actor) error, receipt func(stored *ateapi.Actor) *ateapi.LifecycleReceipt) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	stored, err := getActorRow(ctx, tx, actorRef, true)
	if err != nil {
		return nil, nil, err
	}
	if err := check(stored); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	r := proto.CloneOf(receipt(stored))
	if r.GetHold().GetHolder() == "" || r.GetHold().GetRequestId() != r.GetRequestId() {
		return nil, nil, fmt.Errorf("hold receipt for actor %s names no holder or another request", actorRef)
	}
	r.Hold.CreateTime = timestamppb.New(now)
	r.CreateTime = timestamppb.New(now)
	r.ExpireTime = timestamppb.New(now.Add(store.LifecycleReceiptRetention))
	b, err := proto.Marshal(r)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling hold receipt: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO actor_holds (atespace, actor_name, holder, request_id, receipt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		actorRef.Atespace, actorRef.Name, r.GetHold().GetHolder(), r.GetRequestId(), b)
	if err != nil {
		return nil, nil, fmt.Errorf("inserting hold of %q on actor %s: %w", r.GetHold().GetHolder(), actorRef, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil, store.ErrAlreadyExists
	}
	if stored.Status == nil {
		stored.Status = &ateapi.ActorStatus{}
	}
	stored.Status.Holds = append(stored.Status.Holds, proto.CloneOf(r.GetHold()))
	if r.GetHold().GetHolder() == store.QuarantineHolder {
		if r.GetQuarantine().GetRequestId() != r.GetRequestId() {
			return nil, nil, fmt.Errorf("quarantine receipt for actor %s names another request", actorRef)
		}
		stored.Status.Quarantine = proto.CloneOf(r.GetQuarantine())
		stored.Status.Quarantine.CreateTime = timestamppb.New(now)
	}
	if stored, err = writeActorRow(ctx, tx, actorRef, stored, nil, false); err != nil {
		return nil, nil, err
	}
	if err := insertLifecycleReceipt(ctx, tx, actorRef, r, now); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("committing transaction: %w", err)
	}
	return stored, r, nil
}

func (p *Persistence) ReleaseActorHold(ctx context.Context, actorRef resources.ActorRef, holder, holdRequestID string, check func(*ateapi.Actor) error, receipt func(stored *ateapi.Actor, released *ateapi.ActorHold) *ateapi.LifecycleReceipt) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	stored, err := getActorRow(ctx, tx, actorRef, true)
	if err != nil {
		return nil, nil, err
	}
	if err := check(stored); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	var b []byte
	err = tx.QueryRow(ctx, `
		DELETE FROM actor_holds
		WHERE atespace = $1 AND actor_name = $2 AND holder = $3 AND request_id = $4
		RETURNING receipt`,
		actorRef.Atespace, actorRef.Name, holder, holdRequestID).Scan(&b)
	var released *ateapi.ActorHold
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, nil, fmt.Errorf("deleting hold of %q on actor %s: %w", holder, actorRef, err)
	default:
		placed, err := unmarshalHoldReceipt(b, actorRef)
		if err != nil {
			return nil, nil, err
		}
		released = placed.GetHold()
		stored.Status.Holds = slices.DeleteFunc(stored.Status.Holds, func(h *ateapi.ActorHold) bool {
			return h.GetHolder() == holder && h.GetRequestId() == holdRequestID
		})
		if holder == store.QuarantineHolder {
			stored.Status.Quarantine = nil
		}
		if stored, err = writeActorRow(ctx, tx, actorRef, stored, nil, false); err != nil {
			return nil, nil, err
		}
		// A retry of the hold returns its receipt for the receipt retention
		// after the release, instead of placing the hold again.
		if err := keepHoldReceipt(ctx, tx, actorRef, placed, now); err != nil {
			return nil, nil, err
		}
	}

	r := proto.CloneOf(receipt(stored, released))
	r.CreateTime = timestamppb.New(now)
	r.ExpireTime = timestamppb.New(now.Add(store.LifecycleReceiptRetention))
	if err := insertLifecycleReceipt(ctx, tx, actorRef, r, now); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("committing transaction: %w", err)
	}
	return stored, r, nil
}

func (p *Persistence) ReplaceActorHoldReceipt(ctx context.Context, actorRef resources.ActorRef, r *ateapi.LifecycleReceipt) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	b, err := proto.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling hold receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE actor_holds SET receipt = $5
		WHERE atespace = $1 AND actor_name = $2 AND holder = $3 AND request_id = $4`,
		actorRef.Atespace, actorRef.Name, r.GetHold().GetHolder(), r.GetRequestId(), b); err != nil {
		return fmt.Errorf("replacing receipt of hold %q on actor %s: %w", r.GetRequestId(), actorRef, err)
	}
	if err := keepHoldReceipt(ctx, tx, actorRef, r, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// keepHoldReceipt stores the receipt of a released hold under its request_id,
// replacing an expired copy, and keeps the row LifecycleReceiptRetention
// after now. The receipt itself is the original, expire_time included.
func keepHoldReceipt(ctx context.Context, tx pgx.Tx, actorRef resources.ActorRef, r *ateapi.LifecycleReceipt, now time.Time) error {
	b, err := proto.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling hold receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO actor_lifecycle_receipts (atespace, actor_name, request_id, expire_time, proto)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (atespace, actor_name, request_id) DO UPDATE
			SET expire_time = EXCLUDED.expire_time, proto = EXCLUDED.proto`,
		actorRef.Atespace, actorRef.Name, r.GetRequestId(), now.Add(store.LifecycleReceiptRetention), b); err != nil {
		return fmt.Errorf("storing receipt of released hold %q on actor %s: %w", r.GetRequestId(), actorRef, err)
	}
	return nil
}

func unmarshalHoldReceipt(b []byte, actorRef resources.ActorRef) (*ateapi.LifecycleReceipt, error) {
	r := &ateapi.LifecycleReceipt{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("unmarshaling hold receipt for actor %s: %w", actorRef, err)
	}
	return r, nil
}

// cloneHolds deep-copies holds, so a write can restore them after a mutate.
func cloneHolds(holds []*ateapi.ActorHold) []*ateapi.ActorHold {
	if holds == nil {
		return nil
	}
	out := make([]*ateapi.ActorHold, len(holds))
	for i, h := range holds {
		out[i] = proto.CloneOf(h)
	}
	return out
}
