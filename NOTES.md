# NOTES: upstream/3065-conditional-lifecycle

Branch notes. This file is not part of the patch: drop this commit before
opening a PR. Base: `upstream/1041-activation-id` without its NOTES commit.
It needs the activation ID for `expected_activation`, so it opens after the
#1041 PR.

## Draft PR

**Title:** Check run preconditions and store receipts on Suspend, Resume and Delete

**Description:**

Fixes #3065. A caller that suspends or resumes an actor by name can act on a
later run, or on a replacement actor with the same name, and a retried request
can't tell what its first attempt did. This adds optional preconditions on
the exact run and a durable receipt per request.

- **API.** `SuspendActorRequest` and `ResumeActorRequest` take optional
  `expected_uid`, `expected_activation` and `request_id`. `DeleteOptions`
  takes `expected_activation`, which only `DeleteActor` accepts. Both
  responses carry a `LifecycleReceipt` when the request set `request_id`.
  Callers that set none of the fields keep today's behavior, including the
  lease-free RUNNING fast path in `ResumeActor`.
- **Checks.** UID and activation are checked under the actor's lease before
  any write, and again inside the write that marks the actor SUSPENDING or
  RESUMING. For a suspend, that is before the checkpoint. Delete checks
  `expected_activation` in the `MarkActorForDeletion` mutate. A mismatch
  returns `FAILED_PRECONDITION` and writes nothing.
- **Receipts.** With a `request_id`, the receipt is stored in the same
  transaction as the transition write, and replaced in the write that
  finishes it. The resume's bind and the suspend's release keep their single
  transaction: `BindActorToWorkerWithReceipt` and
  `UpdateActorAndReleaseWithReceipt` store the receipt with them. A call that
  finds the actor already SUSPENDED or RUNNING stores a receipt with
  `transitioned` false, so its retry never acts on a later activation.
- **Retry.** The same `request_id` and parameters return the stored receipt
  with no `actor`, and do nothing else, even after the actor moved to another
  activation or was replaced. Other parameters return `ALREADY_EXISTS`. A
  receipt left SUSPENDING or RESUMING is finished by its retry while the actor
  is still in that state under the same UID and activation.
- **Storage.** Migration `000002` adds `actor_lifecycle_receipts(atespace,
  actor_name, request_id)`. Rows are kept 24 hours, survive `DeleteActor`, and
  go with the atespace. The maintenance loop deletes expired rows.

Decisions on the issue's open questions: a non-empty `expected_activation`
never matches a SUSPENDED actor, which holds none; receipts are keyed by actor
name so they outlive a replacement; `RevertActor` is unchanged.

Version skew: ate-api-server rejects unknown request fields, so clients send
the new fields only after every replica has rolled. The migration adds a table
and keeps working for the previous binary.

AI use: I used Claude Code to port this change from a downstream patch and to
draft tests and this description. I reviewed the diff and ran the checks
below.

Tests: `functionaltest/lifecycle_receipt_test.go` through the gRPC service
(stale run, replacement, retry, an interrupted suspend finished by its retry,
conflicting reuse, async suspend, `expected_activation` refused on other
deletes); `TestAssignWorkerAttempt_ChecksConditionsInWrite`; store contract
rows for create and replace in one transaction, nil mutate, failures storing
nothing, receipts outliving `DeleteActor`, and the bind and release variants;
`atepg/TestLifecycleReceipt_Expiry`.

## Changes from the downstream patch

- rc1 wrote RESUMING in a second `UpdateActor` and released the worker before
  the SUSPENDED write. Main does both in one transaction (#3223), so the
  receipt now commits through two new store methods,
  `BindActorToWorkerWithReceipt` and `UpdateActorAndReleaseWithReceipt`, with
  contract tests for both. `lifecycleCall.write` picks the plain or receipt
  variant for `updateActor`, `bindActorToWorker` and `updateActorAndRelease`.
- atepg's `updateActor` is split into `getActorRow` and `writeActorRow`, both
  taking main's `querier`, so the receipt write and the bind share a
  transaction.
- The service layer keeps main's `actorWriteError` and
  `validatedActorMutation`; the fork's `actorUpdateError` is gone.
- The known limit "the in-write check runs after `BindActorToWorker`" no longer
  applies: a refused check rolls the claim back.
- The PR is large (about 3.3k lines, 1.2k of it non-test, non-generated). It
  may need to split: API and store, then the workflows.

## Known limits

- `replaceLifecycleReceipt` reports an expired receipt as
  `ErrVersionConflict`. The bind variant returns it unmapped, so
  `ensureWorkerAssigned` retries until its backoff runs out and then returns
  Aborted. Reaching it takes a continuing call whose receipt expires mid-call
  while the actor is SUSPENDED. The client's retry recovers.
- Same as the downstream patch: every ateapi replica must run this before
  clients send the fields; receipt expiry uses ate-api-server's clock.

## Open questions for review

- One store method per transition, or one optional receipt argument on
  `BindActorToWorker` and `UpdateActorAndRelease`.
- Retention of 24 hours, and the clock it uses (ate-api-server's).

## Evidence

See `## Verification` below, filled from the local run.

## Verification

macOS arm64, Go 1.27.3, Docker for PostgreSQL. Patch head `38a840e3c`, on main `94e0a26ac`.

- `hack/verify-all.sh` with GNU sed first on `PATH`: passes.
- `hack/verify/postgresql-migrations.sh origin/main`: passes.
- `go test -race -count=1` over every Go package the branch changes: 12 packages pass. `internal/atunnel` is Linux-only and is excluded on darwin.
- Every commit builds and passes `go vet ./cmd/... ./internal/...`.
- New store contract rows `BindAndReleaseWithReceipt` and `BindActorToWorkerWithReceipt_FailureStoresNothing` pass on PostgreSQL. With the bind's receipt write removed, `BindAndReleaseWithReceipt` fails.
- The new controlapi, functional, atepg contract, atenet egress and `substratex509` tests pass with `-race -count=3 -shuffle=on`. They ran on the prototype tip `f2de9b9a5`, which contains every branch's tests.
