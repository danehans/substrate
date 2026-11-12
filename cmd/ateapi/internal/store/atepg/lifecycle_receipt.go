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
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (p *Persistence) GetActorLifecycleReceipt(ctx context.Context, actorRef resources.ActorRef, requestID string) (*ateapi.LifecycleReceipt, error) {
	receipt, err := getLifecycleReceipt(ctx, p.pool, actorRef, requestID, time.Now(), false)
	if !errors.Is(err, store.ErrNotFound) {
		return receipt, err
	}
	// Active holds retain their request IDs beyond ordinary receipt expiry.
	var b []byte
	err = p.pool.QueryRow(ctx, `SELECT receipt FROM actor_holds
  WHERE atespace = $1 AND actor_name = $2 AND request_id = $3`,
		actorRef.Atespace, actorRef.Name, requestID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading active hold receipt: %w", err)
	}
	return unmarshalHoldReceipt(b, actorRef)
}

func (p *Persistence) UpdateActorWithReceipt(ctx context.Context, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error, receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	if err := precondition.Validate(); err != nil {
		return nil, nil, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	stored, err := getActorRow(ctx, tx, actorRef, true)
	if err != nil {
		return nil, nil, err
	}
	if err := store.DeletePreconditions(precondition).Check(stored.GetMetadata()); err != nil {
		return nil, nil, err
	}
	if mutate != nil {
		if stored, err = writeActorRow(ctx, tx, actorRef, stored, mutate, false); err != nil {
			return nil, nil, err
		}
	}

	r, err := storeLifecycleReceipt(ctx, tx, actorRef, stored, receipt, create)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("committing transaction: %w", err)
	}
	return stored, r, nil
}

// storeLifecycleReceipt stores the receipt built from the Actor as stored, in
// tx. create inserts it; otherwise it replaces the stored one.
func storeLifecycleReceipt(ctx context.Context, tx pgx.Tx, actorRef resources.ActorRef, stored *ateapi.Actor, receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.LifecycleReceipt, error) {
	r := proto.CloneOf(receipt(stored))
	now := time.Now()
	var err error
	if create {
		r.CreateTime = timestamppb.New(now)
		r.ExpireTime = timestamppb.New(now.Add(store.LifecycleReceiptRetention))
		err = insertLifecycleReceipt(ctx, tx, actorRef, r, now)
	} else {
		err = replaceLifecycleReceipt(ctx, tx, actorRef, r, now)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// getLifecycleReceipt reads the receipt for requestID that has not expired by
// now. forUpdate locks the row until the transaction q runs in ends.
func getLifecycleReceipt(ctx context.Context, q querier, actorRef resources.ActorRef, requestID string, now time.Time, forUpdate bool) (*ateapi.LifecycleReceipt, error) {
	query := `
		SELECT proto FROM actor_lifecycle_receipts
		WHERE atespace = $1 AND actor_name = $2 AND request_id = $3 AND expire_time > $4`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var b []byte
	if err := q.QueryRow(ctx, query, actorRef.Atespace, actorRef.Name, requestID, now).Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("getting lifecycle receipt %q for actor %s: %w", requestID, actorRef, err)
	}
	// A receipt has no defaults to backfill. Fields a newer replica wrote are
	// dropped, as unmarshalStored does for resources.
	r := &ateapi.LifecycleReceipt{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("unmarshaling lifecycle receipt %q for actor %s: %w", requestID, actorRef, err)
	}
	return r, nil
}

// insertLifecycleReceipt stores r, replacing an expired receipt under the
// same key. An unexpired one fails the insert with ErrAlreadyExists.
func insertLifecycleReceipt(ctx context.Context, tx pgx.Tx, actorRef resources.ActorRef, r *ateapi.LifecycleReceipt, now time.Time) error {
	// Callers hold the actor row lock, which also serializes hold changes.
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM actor_holds
  WHERE atespace = $1 AND actor_name = $2 AND request_id = $3)`,
		actorRef.Atespace, actorRef.Name, r.GetRequestId()).Scan(&held); err != nil {
		return fmt.Errorf("checking active hold request: %w", err)
	}
	if held {
		return store.ErrAlreadyExists
	}

	b, err := proto.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling lifecycle receipt: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO actor_lifecycle_receipts (atespace, actor_name, request_id, expire_time, proto)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (atespace, actor_name, request_id) DO UPDATE
			SET expire_time = EXCLUDED.expire_time, proto = EXCLUDED.proto
			WHERE actor_lifecycle_receipts.expire_time <= $6`,
		actorRef.Atespace, actorRef.Name, r.GetRequestId(), r.GetExpireTime().AsTime(), b, now)
	if err != nil {
		return fmt.Errorf("inserting lifecycle receipt %q for actor %s: %w", r.GetRequestId(), actorRef, err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrAlreadyExists
	}
	return nil
}

// replaceLifecycleReceipt overwrites the unexpired receipt for r's request,
// keeping its create_time and expire_time. A receipt that expired since the
// caller read it fails with ErrVersionConflict, so the caller retries from
// a fresh lookup.
func replaceLifecycleReceipt(ctx context.Context, tx pgx.Tx, actorRef resources.ActorRef, r *ateapi.LifecycleReceipt, now time.Time) error {
	prev, err := getLifecycleReceipt(ctx, tx, actorRef, r.GetRequestId(), now, true)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("lifecycle receipt %q for actor %s is gone: %w", r.GetRequestId(), actorRef, store.ErrVersionConflict)
	}
	if err != nil {
		return err
	}
	r.CreateTime, r.ExpireTime = prev.GetCreateTime(), prev.GetExpireTime()
	b, err := proto.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling lifecycle receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE actor_lifecycle_receipts SET proto = $4
		WHERE atespace = $1 AND actor_name = $2 AND request_id = $3`,
		actorRef.Atespace, actorRef.Name, r.GetRequestId(), b); err != nil {
		return fmt.Errorf("replacing lifecycle receipt %q for actor %s: %w", r.GetRequestId(), actorRef, err)
	}
	return nil
}

// deleteExpiredLifecycleReceipts deletes the receipts of every atespace whose
// expire_time has passed, so it reads every partition by definition.
const deleteExpiredLifecycleReceipts = `DELETE FROM actor_lifecycle_receipts WHERE expire_time <= $1`

// cleanupExpiredLifecycleReceipts deletes receipts whose expire_time has
// passed and returns how many it deleted.
func (p *Persistence) cleanupExpiredLifecycleReceipts(ctx context.Context) (int64, error) {
	tag, err := p.watchPool.Exec(ctx, deleteExpiredLifecycleReceipts, time.Now())
	if err != nil {
		return 0, fmt.Errorf("deleting expired lifecycle receipts: %w", err)
	}
	return tag.RowsAffected(), nil
}
