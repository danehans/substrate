# NOTES: upstream/1041-activation-id

Branch notes. This file is not part of the patch: drop this commit before
opening a PR. Base: `agent-substrate/substrate` main at `94e0a26ac`.

## Draft PR

**Title:** Record an activation ID on each resume

**Description:**

Part of #1041. An actor's UID doesn't say which run of it sent a request or
crashed. This adds an activation ID that names one Run or Restore of an actor,
as discussed in the #1041 design doc review (one ID per activation, named
`ate.actor.activation`).

- `BindActorToWorker` mints a ULID in the write that sets RESUMING and the
  worker assignment, and stores it as the output-only
  `ActorStatus.activation_id`. Every write that clears the assignment clears it
  too, through `clearWorkerAssignment`.
- A resume that finds the actor RESUMING replaces the ID before it restores
  again (`rotateActivation`), so an ID is never sent on two Run or Restore
  calls. This is the only write the change adds, and only on that path.
- `finalizeRunning` commits RUNNING only if the actor still holds the ID it
  restored under, and returns `Aborted` otherwise.
- The ID rides `RunRequest`/`RestoreRequest` (atelet) and
  `RunWorkloadRequest`/`RestoreWorkloadRequest` (worker). It is recorded on
  `ActorCrash.activation_id` and is the `ate.actor.activation` attribute on
  ateapi spans, `ate.actor.state_changed`, `ate.actor.crashed` and the
  worker's actor logs.

The first resume attempt adds no write, RPC or round trip.

Version skew: `ateworker` rejects fields it doesn't know, and atelet can be one
minor release newer than the workers. So atelet must start sending
`activation_id` to the worker only in the minor release after the worker learns
it. ateapi to atelet is fine, because atelet upgrades first. I'd like a
maintainer's view on how to stage this: hold the atelet-to-worker send behind a
release, or split the worker proto change into its own PR first.

AI use: I used Claude Code to port this change from a downstream patch on
`v1.0.0-rc1` to main and to draft tests and this description. I reviewed the
diff and ran the checks below.

Tests:

- `TestResumeActor_Activation`: first attempt and re-entry, counting writes and
  checking the ID sent to atelet.
- `TestClearWorkerAssignment`.
- `functionaltest/activation_test.go`: resume, suspend and resume through the
  gRPC service, a retry after a failed Run, and the crash record.
- Existing tests updated for the extra `newActorCrash` argument and the
  restored activation.

## Changes from the downstream patch

- Minted in `BindActorToWorker`'s mutate. Main's #3223 writes RESUMING and the
  claim in one transaction, so the rc1 split (claim, then a second
  `UpdateActor`) and its `workerHoldingStaleClaim` recovery are gone. The test
  row for "claim left between the two writes" is removed.
- The rotate path stays. Main still re-enters a RESUMING actor after a failed
  Run or Restore (`validateAssignedWorker`) without a write, and that attempt's
  Run may have reached the worker. Without the rotation the next Run would
  reuse the ID.
- Main's new `UpdateActorAndRelease` paths (crash, worker delete, delete,
  revert, suspend) clear the activation through `clearWorkerAssignment`,
  including the DELETING branch of `crashBoundActor`.
- New files use the "The Agent Substrate Authors" header (#3644).

## Open questions for review

- Version skew staging, above.
- Per-resume cost: Tim asked about it on the design doc. The ID is one field on
  writes and requests that already happen.

## Evidence

See `## Verification` below, filled from the local run.

## Verification

macOS arm64, Go 1.27.3, Docker for PostgreSQL. Patch head `dfd06de5a`, on main `94e0a26ac`.

- `hack/verify-all.sh` with GNU sed first on `PATH`: passes.
- `hack/verify/postgresql-migrations.sh origin/main`: passes.
- `go test -race -count=1` over every Go package the branch changes: 10 packages pass. `internal/atunnel` is Linux-only and is excluded on darwin.
- Every commit builds and passes `go vet ./cmd/... ./internal/...`.
- The new controlapi, functional, atepg contract, atenet egress and `substratex509` tests pass with `-race -count=3 -shuffle=on`. They ran on the prototype tip `f2de9b9a5`, which contains every branch's tests.
