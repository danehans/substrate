# danehans/substrate patches

`danehans/substrate` is an experimental, unqualified downstream of
`kagent-dev/substrate`. Every release is built from one commit on a
`danehans/<base tag>` branch, and each change below that is not in the base
names its upstream proposal and the condition that retires it.
[`fork.env`](fork.env) records the base.

## Functional patches

1. **Activation ID.** Each resume that writes RESUMING mints a ULID in that
   write, `ActorStatus.activation_id`. It is set and cleared in the same
   writes as `worker_assignment`, so a resume that stops between the worker
   claim and that write leaves none, and the next resume mints a fresh one. A
   resume that finds the Actor already RESUMING replaces the ID before it
   restores again, in one extra write on that path only. RUNNING is committed
   only if the Actor still holds the ID that Run or Restore was sent with. The
   ID rides Run and Restore to atelet and the worker, is recorded on
   `ActorCrash.activation_id`, and is the `ate.actor.activation` attribute on
   ateapi spans, lifecycle events and worker logs. The invariant holds only
   when every ateapi replica runs this patch: the store drops unknown fields,
   so a write by a base replica strips the ID from an assigned Actor. The
   next resume of that Actor refuses RUNNING and replaces the ID.
   - Upstream proposal:
     [agent-substrate/substrate#1041](https://github.com/agent-substrate/substrate/issues/1041).
     Upstream main writes RESUMING with the `BindActorToWorker` claim in one
     transaction (agent-substrate/substrate#3223), so the rebased patch mints
     in that write, and a resume can no longer stop between claim and
     RESUMING.
   - Replacement condition: a `kagent-dev/substrate` release carries an
     activation ID with these semantics, and Picket's `substrate` runtime pins
     and qualifies it
     ([solo-io/picket#15](https://github.com/solo-io/picket/issues/15)).
2. **Certificate binding.** Worker-for-actor certificates are bound to one
   activation. atunnel keeps the activation it was given on Run or Restore
   and sends it as `activation_id` on `MintActorCertificate` with the first
   mint and every renewal; atelet forwards it on
   `MintWorkerActorCertificate`. ateapi refuses the mint with `ABORTED`
   unless it equals the Actor's current `activation_id` (an empty ID matches
   only an Actor with none), which also ends atunnel's renewal loop. The
   issued certificate's `ActorIdentity` extension carries it as
   `ActivationId`, omitted when empty. atenet now checks the extension's UID
   and optional activation against the Actor on every CONNECT. The base
   release issues the `ActorIdentity` extension without `ActivationId`, so
   those certificates receive the UID check only. A missing or malformed
   extension is refused. The chart's agentgateway still identifies the actor
   from the URI SAN and does not apply this CONNECT check.
   - Upstream proposal:
     [agent-substrate/substrate#1041](https://github.com/agent-substrate/substrate/issues/1041)
     (the certificate half of the authenticated per-run identity).
   - Replacement condition: a `kagent-dev/substrate` release issues
     activation-bound worker-for-actor certificates with these semantics, and
     Picket's `substrate` runtime pins and qualifies it
     ([solo-io/picket#15](https://github.com/solo-io/picket/issues/15)).

3. **Actor quarantine.** `QuarantineActor` places a hold with the holder
   `quarantine` and sets `ActorStatus.quarantine` in one write, then suspends
   a `RUNNING` actor at the expected activation. While quarantined, ateapi
   refuses resume, delete, revert and tagging, refuses worker-for-actor
   certificates and actor JWTs, keeps replaced snapshots, and fails
   `GetActorEgressPolicy`, which agentgateway denies on for every request and
   TLS handshake. atenet refuses new tunnels. `ReleaseQuarantine` takes the
   new `quarantine_admin` role. It builds on the actor holds from
   [agent-substrate/substrate#3066](https://github.com/agent-substrate/substrate/issues/3066). See
   [actor quarantine](../../../docs/guides/actor-quarantine.md).
   - Upstream proposal:
     [agent-substrate/substrate#3463](https://github.com/agent-substrate/substrate/issues/3463).
   - Replacement condition: a `kagent-dev/substrate` release quarantines
     actors with these semantics, and Picket's `substrate` runtime pins and
     qualifies it
     ([solo-io/picket#15](https://github.com/solo-io/picket/issues/15)).

## Packaging changes

1. **Release publishing.** The release workflow publishes this fork's images
   (`linux/amd64` and `linux/arm64`), Helm charts (OCI) and `kubectl-ate`
   binaries under `ghcr.io/danehans/substrate`, and the charts default
   `image.repository` to `danehans/substrate`. Tags must be
   `<base tag>-danehans.N`.
   - Upstream proposal: none; this is downstream-only packaging.
   - Replacement condition: removed with the fork, once a `kagent-dev/substrate`
     release ships the contracts this fork carries and Picket's `substrate`
     runtime pins and qualifies it
     ([solo-io/picket#15](https://github.com/solo-io/picket/issues/15)).
2. **Egress gateway default.** `images.agentgateway` defaults to a multi-arch
   build of `danehans/agentgateway` at the revision in
   [`agentgateway.env`](agentgateway.env), built by this fork's
   `agentgateway-image` workflow and pinned by digest. The chart's
   `atenet-egress` gateway and `atenet-router` ingress router both use this
   value, as they do in the base. The kustomize manifests under
   `manifests/ate-install` keep the base gateway.
   - Upstream proposal:
     [agentgateway/agentgateway#4289](https://github.com/agentgateway/agentgateway/issues/4289)
     (expose the applied Substrate egress policy's UID and version to CEL and
     ext_proc).
   - Replacement condition: an agentgateway release contains the change and
     a `kagent-dev/substrate` release bundles that gateway; the default then
     returns to the base chart's gateway.
