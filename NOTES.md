# NOTES: upstream/3066-actor-holds

Branch notes. This file is not part of the patch: drop this commit before
opening a PR. Base: `upstream/3065-conditional-lifecycle` without its NOTES
commit. Holds reuse its receipts and `request_id` space.

## Draft PR

**Title:** Add holder-scoped actor holds

**Description:**

Fixes #3066. A controller that has stopped an actor can't keep it stopped:
any caller, including the router's ingress resumer, can resume it. This adds
holds that block every resume until each holder releases its own. It follows
JeffLuoo's review on #3066: many holders, each released separately, all listed
on the actor.

- **API.** `HoldActor` places the caller's hold, with a `reason`, an optional
  `incident_id`, optional `expected_uid` and `expected_activation`, and a
  required `request_id` that names the hold. `ReleaseActorHold` releases a hold
  the caller placed, named by `hold_request_id`. `ActorStatus.holds` lists
  active holds. `LifecycleReceipt` gains the hold and release operations.
- **Holder.** The holder is `user:` and the authenticated caller's ID, as
  AccessPolicy members name it. Each holder has at most one hold on an actor,
  and an actor has at most 16. Only the holder can release its hold.
- **Resume.** `ResumeActor` refuses a held actor, RUNNING or not: on the
  lease-free fast path, after the lease, and in the RESUMING and RUNNING
  writes. The template reconciler calls the workflow without the authz
  interceptor and is refused the same way. A hold refused in the bind rolls the
  claim back. A hold placed during a restore leaves the actor RESUMING and
  terminates the restored workload. A resume after the last release restores it
  again under a new activation.
- **Ingress.** The refusal is `FAILED_PRECONDITION` with
  `ErrorInfo{domain: "ate.dev", reason: "TERMINAL_STATE", metadata: {"cause":
  "ACTOR_HELD"}}`. The atenet resumer already fails fast on
  `FAILED_PRECONDITION` (#3472), and agentgateway's Substrate ingress returns
  `TERMINAL_STATE` without parking.
- **Delete.** `DeleteActor` on a held actor returns the same error, so its
  snapshots stay until every hold is released. The store also refuses:
  `actor_holds` has a foreign key to `actors` with `ON DELETE RESTRICT`.
- **Lifecycle.** Only the hold writes change `status.holds`. Suspend, async
  upload reports and `RevertActor` keep them, and `CreateActor` clears them, so
  a replacement under the same name starts with none.
- **Receipts.** Hold and release receipts follow #3065's rules. A hold's
  receipt is also kept in its `actor_holds` row, so a retry returns it while
  the hold exists.
- **Authz.** New `can_hold` and `can_release_hold` relations: atespace owner
  or the new `hold_manager` role, not editor.
- **Storage.** Migration `000003` adds `actor_holds`.
- **Audit.** Each hold placed and released writes `ate.actor.hold_changed`
  with the caller, holder, reason, incident ID and request IDs.

Decisions on the issue's open questions: delete is refused while held; one hold
per holder; a hold does not block egress certificates, because it doesn't stop
a running actor.

AI use: I used Claude Code to port this change from a downstream patch and to
draft tests and this description. I reviewed the diff and ran the checks
below.

Tests: `functionaltest/actor_hold_test.go` (resume refused for SUSPENDED and
RUNNING actors with the exact `ErrorInfo`, only the holder releases, two
holders, retries, delete refused, replacement not held, `expected_activation`,
holds survive revert and upload, a hold during restore);
`TestReconcileOne_HeldGoldenActorIsNotResumed`;
`TestEnsureWorkerAssigned_HeldAfterRead`; store contract rows;
`model_test.fga.yaml` rows for owners, `hold_manager`, editors, viewers,
workload identities and the router.

## Changes from the downstream patch

- Dropped the atenet resumer commit: main fails fast on every
  `FAILED_PRECONDITION` since #3472.
- Dropped the explicit claim release after a refused RESUMING write: main's
  bind is one transaction, so the refusal rolls the claim back.
- `DeleteActor` in atepg checks `status.holds` on the row it locked, as main
  checks the worker assignment, and maps the foreign key violation from the
  `DELETE` as a backstop. The rc1 patch mapped it at a step main no longer has.
- About 4k lines, 1.1k non-test, non-generated. It may need to split: API and
  store, then enforcement.

## Open questions for review

- JeffLuoo's second point on #3066, a combined hold-and-suspend call under the
  hold permission, isn't here. The #3463 prototype's `QuarantineActor` is that
  call for quarantine.
- No role can release another principal's hold. A hold whose holder can no
  longer call ateapi needs a database edit.
- `status.holds`, with reasons and incident IDs, is visible to every viewer.

## Evidence

See `## Verification` below, filled from the local run.

## Verification

macOS arm64, Go 1.27.3, Docker for PostgreSQL. Patch head `0cdc9790e`, on main `94e0a26ac`.

- `hack/verify-all.sh` with GNU sed first on `PATH`: passes.
- `hack/verify/postgresql-migrations.sh origin/main`: passes.
- `go test -race -count=1` over every Go package the branch changes: 14 packages pass. `internal/atunnel` is Linux-only and is excluded on darwin.
- Every commit builds and passes `go vet ./cmd/... ./internal/...`.
- The new controlapi, functional, atepg contract, atenet egress and `substratex509` tests pass with `-race -count=3 -shuffle=on`. They ran on the prototype tip `f2de9b9a5`, which contains every branch's tests.
