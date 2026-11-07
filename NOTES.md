# NOTES: upstream/certificate-binding

Branch notes. This file is not part of the patch: drop this commit before
opening a PR. Base: `upstream/1041-activation-id` without its NOTES commit.
Upstream has no `docs/upstream/` directory, so the issue draft is below.

## Draft upstream issue (not filed)

**Title:** Bind worker-for-actor certificates to the actor's activation

#1041 proposes an activation ID that names one Run or Restore of an actor.
This issue proposes the certificate half: a worker-for-actor certificate names
the activation it was minted for, and the control plane mints only for the
actor's current activation.

**Problem**

`MintWorkerActorCertificate` checks the node, the worker assignment and the
actor UID. It doesn't tell runs of one actor apart. After a suspend and resume,
or a retried resume, onto the same worker, a renewal loop started for the
earlier run keeps getting certificates, and they look the same as the current
run's. A consumer of egress traffic, such as a policy engine, an audit pipeline
or a containment system, can identify the actor but not the run that sent a
request. So it can't attribute or act on one run without affecting the next.

**Proposal**

1. Add optional `activation_id` to `MintActorCertificateRequest` (atelet) and
   `MintWorkerActorCertificateRequest` (ateapi). atunnel keeps the activation
   from Run or Restore and sends it on the first mint and every renewal. atelet
   forwards it.
2. In `MintWorkerActorCertificate`, after the existing checks, refuse with
   `ABORTED` unless `activation_id` equals `ActorStatus.activation_id`.
   `ABORTED` ends atunnel's renewal loop; `NOT_FOUND` would retry. An empty ID
   matches only an actor with none.
3. Add `ActivationId` to the `ActorIdentity` extension, omitted when empty, so
   certificates for actors without an activation are unchanged.

**Compatibility**

- The extension is JSON. `ActorIdentityFromCertificate` ignores unknown keys.
- The atenet egress router and agentgateway's `EgressActorResolution` name the
  actor from the URI SAN and don't read the extension, so they resolve a
  certificate with `ActivationId` as before.
- Version skew: atelet can be one release newer than ate-api-server, and
  ate-api-server rejects unknown request fields. So atelet sends
  `activation_id` to ate-api-server only from the release after ate-api-server
  learns it. The worker sends it to atelet, which upgrades first.
- With main's single-write resume (#3223), the activation is set in the same
  write as `WorkerAssignment`, so a mint never sees an assignment without its
  activation.

**Out of scope**

Revoking a certificate already issued (it expires within an hour), and having
the egress PEP compare the activation with the actor. Both could follow; the
second is prototyped for atenet in the #3463 prototype branch.

**Reference implementation:** `danehans/substrate` branch
`upstream/certificate-binding`, on main.

## Draft PR (after the issue is filed)

**Title:** Bind worker-for-actor certificates to one activation

**Description:**

Fixes #NNNN (the issue above). Builds on #1041's activation ID (PR above).

- atunnel sends the activation it was given on Run or Restore as
  `activation_id` on `MintActorCertificate`, with the first mint and every
  renewal. atelet forwards it on `MintWorkerActorCertificate`.
- ateapi mints only while the actor still holds that activation, and otherwise
  refuses with `Aborted`, which ends atunnel's renewal loop.
- The certificate's `ActorIdentity` extension carries it as `ActivationId`,
  omitted when empty, so a certificate for an actor without one encodes as
  before.

| Actor holds | Worker sends | Result |
| --- | --- | --- |
| A | A | Issued; certificate names A |
| none | none | Issued; no `ActivationId` |
| B | A | `Aborted`; renewal loop ends |
| A | B or none | `Aborted` |
| none | A | `Aborted` |

AI use: I used Claude Code to port this change from a downstream patch and to
draft tests and this description. I reviewed the diff and ran the checks below.

Tests: `TestMintWorkerActorCertificate_Activation` and
`_RenewalWithinActivation` (workerservice);
`TestActivation_CertificateFollowsActivation` and
`_CertificateRefusedAfterReentry` (functional, real resume and suspend);
atunnel sends the activation on every mint; atelet forwards it;
`substratex509` encodes it exactly and bounds its length; atenet resolves a
certificate that carries it.

## Changes from the downstream patch

None in the code. The cherry-pick applied cleanly on main; only headers and
the commit message changed.

## Open questions for review

- Version skew staging for the atelet-to-ateapi field, above.

## Evidence

See `## Verification` below, filled from the local run.

## Verification

macOS arm64, Go 1.27.3, Docker for PostgreSQL. Patch head `8ab640dfe`, on main `94e0a26ac`.

- `hack/verify-all.sh` with GNU sed first on `PATH`: passes.
- `hack/verify/postgresql-migrations.sh origin/main`: passes.
- `go test -race -count=1` over every Go package the branch changes: 15 packages pass. `internal/atunnel` is Linux-only and is excluded on darwin.
- Every commit builds and passes `go vet ./cmd/... ./internal/...`.
- Linux-only packages in `golang:1.27.3` with `--privileged --tmpfs /tmp:exec`: `go test -race ./internal/atunnel/... ./cmd/ateworker-gvisor/... ./cmd/ateworker-microvm/...` passes (14 packages).
- The new controlapi, functional, atepg contract, atenet egress and `substratex509` tests pass with `-race -count=3 -shuffle=on`. They ran on the prototype tip `f2de9b9a5`, which contains every branch's tests.
