# Actor quarantine

Quarantine contains a misbehaving actor in one call and keeps its state for
investigation. It is built from [actor holds](../reference/api-guide.md#actor-holds):
a quarantine is a hold with the holder `quarantine`, plus the restrictions
below. This guide is the supported playbook: what stops at once, what can last
up to a cache TTL, and what a quarantine does not cover. The API is in
[Actor quarantine](../reference/api-guide.md#actor-quarantine).

## Quarantine an actor

Call `QuarantineActor` with:

- `reason`, and optionally `incident_id`;
- `expected_uid`, so a replacement actor under the same name is never
  quarantined. Set it: without it, a reused `request_id` returns the earlier
  actor's receipt for 24 hours, and after that quarantines whichever actor
  has the name;
- `expected_activation`, the run you observed misbehaving, if the actor is
  running;
- `request_id`, which names the quarantine and makes the call safe to retry.

The caller needs `can_quarantine`: an editor or owner of the atespace, or the
`quarantine_manager` or `quarantine_admin` role. See
[authorization](authorization.md).

ateapi then:

1. In one write, places the quarantine hold, sets `status.quarantine` and
   stores the receipt. A stale `expected_uid` or `expected_activation` fails
   with `FAILED_PRECONDITION` and changes nothing.
2. If the actor is `RUNNING`, suspends that activation, with an asynchronous
   suspend under the `request_id` and `/suspend`, and replaces the receipt
   with one that adds `QUARANTINE_STEP_SUSPENDED`.

The receipt's `steps` say what took effect. If the suspend fails, the call
returns the error, the quarantine stays, and a retry with the same
`request_id` finishes the suspend.

## What stops at the quarantine write

- **Resume, from every caller.** `ResumeActor` fails with
  `FAILED_PRECONDITION`, `ErrorInfo` reason `TERMINAL_STATE` and cause
  `ACTOR_QUARANTINED`, whether the actor is suspended or running. This
  includes restores, the template reconciler and both ingress routers, which
  return the error at once instead of parking the request. atenet-router asks
  ateapi on every request, so its new requests are refused at once.
- **Delete, revert and tag.** `DeleteActor`, `RevertActor` and `CreateTag`
  fail the same way, so the snapshots can't be deleted, rolled back or copied
  for another actor to restore.
- **Snapshot collection.** A suspend that replaces a quarantined actor's
  snapshot keeps the replaced one in object storage.
- **New credentials.** `MintWorkerActorCertificate` and `MintActorJwt`, the
  actor JWT the egress gateway injects, are refused. The certificate the
  worker already holds stays valid; see below.
- **New egress through the chart's agentgateway.** `GetActorEgressPolicy`
  fails with `FAILED_PRECONDITION` for every caller. agentgateway fetches the
  policy for every HTTP request and TLS handshake in a tunnel, with no cache,
  and denies on that error. This holds for tunnels opened before the
  quarantine, and for TLS passthrough, whose handshake is checked.
- **New egress tunnels through atenet**, the kustomize gateway. It reads the
  actor for every new tunnel and refuses a quarantined one.

An update to the actor's egress policy is stored but does not lift any of
this: the policy stays unreadable until the quarantine is released.

## What stops when the suspend finishes

The suspend tears down the workload. atunnel closes every egress tunnel,
including TLS passthrough ones, and cancels in-flight ingress requests. The
actor is then `SUSPENDED`, and both gateways refuse new tunnels from it.
This is the only step that closes connections that are already open.

## What can last up to a TTL

Until the suspend finishes:

- agentgateway caches an actor's worker assignment for 5 seconds, so an
  ingress request can still reach a running actor for that long.
- atenet caches egress policies for 10 seconds. Requests in tunnels opened
  before the quarantine use the cached policy for that long.
- atunnel caches the policy it resolves DNS with for 5 seconds, and keeps the
  cached one if a refresh fails.
- Bytes on open tunnels, including TLS passthrough, keep flowing.
- The worker keeps its certificate, which lasts an hour and is renewed at 90%
  of that. The refused renewal ends atunnel's renewals, and atunnel then
  refuses new egress itself, so on the certificate alone that can take up to
  about 54 minutes. The gateways' refusals above don't depend on it.

If the actor was not `RUNNING`, there is nothing to suspend. A `SUSPENDED`
actor has no workload or open connections. A `RESUMING` actor's restore is
stopped by the hold, which terminates the restored workload. A `SUSPENDING`
actor is left to the suspend already under way, and a retry of the quarantine
does not retry that suspend: retry `SuspendActor` with its own `request_id`
if it is stuck. A `REVERTING` actor can't be quarantined until the revert
finishes, because a revert discards snapshots; retry the call.

If the suspend fails, the quarantine stays in place but open connections stay
open until the workload stops. Retry the call. A retry continues the suspend
only while the quarantine is in place, so it never suspends the actor after
the release.

## Evidence

While quarantined, the actor's snapshots are kept: nothing restores, reverts,
tags, deletes or collects them. One race remains: a `CreateTag` that checked
the actor before the quarantine landed finishes its copy. An asynchronous suspend finishes uploading in
the background; the worker still gets the credentials it needs for the
upload. The receipt's `snapshot_durable` says whether the snapshot was durable
when the suspend finished. Snapshots a suspend replaced stay in object
storage under the actor's prefix, no longer listed in `status.snapshots`,
until the actor is deleted after the release.

There is no export or forensics restore yet. To inspect a snapshot, copy it
from object storage with your own credentials into an environment with no
egress.

## Release

`ReleaseQuarantine` names the quarantine by its `request_id`, is idempotent,
and is audited. It needs `can_release_quarantine`, which only the
`quarantine_admin` role grants. Neither the caller that placed the quarantine
nor the actor's editors and owners can release it without that role, and
`ReleaseActorHold` can't remove the quarantine hold. An owner can grant
themselves `quarantine_admin` through an access policy, so keep owner rights
as narrow as release rights.

Releasing does not resume the actor, but it lifts the barrier: the next
routed request or other resume restores it. Its snapshots may still hold the
state that caused the quarantine, so revert or delete the actor if it must
not run that state again.

## Audit

Each quarantine, each suspend it finishes, each release, and each resume a
quarantine refuses writes an `ate.actor.quarantine_changed` event with the
caller, reason, incident ID, request ID, uid, the activation the quarantine
found, and the steps in effect. See `docs/metrics/registry/events.yaml`.
A routed request to a quarantined actor writes one refused-resume event, so
the stream grows with ingress traffic to it.

## Out of scope

- Closing open connections without a suspend. That needs an agentgateway
  change to end tunnels when an actor is quarantined.
- agentgateway reading `status.quarantine` itself. It denies through the
  egress policy fetch instead, and still admits a tunnel from a running
  quarantined actor before denying the requests in it.
- Cancelling upstream work that was already forwarded.
- Quarantining every actor of a template or an atespace.
- Exporting snapshots, or restoring one into an isolated environment.
- `kubectl ate` commands.
- A compromised node, worker or control plane.
- Mixed versions. Every ateapi replica must run this patch before a client
  quarantines an actor. A replica without it resumes, deletes and serves
  egress for the actor.
