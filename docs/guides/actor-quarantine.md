# Actor quarantine

Quarantine places an [actor hold](../reference/api-guide.md#actor-holds),
restricts lifecycle and credential operations, and retains snapshots for
investigation. These downstream controls are experimental, unqualified and
for development only. See the [API contract](../reference/api-guide.md#actor-quarantine)
for receipts and retry behavior.

## Quarantine an actor

Call `QuarantineActor` with `reason`, a stable `request_id`, and optionally
`incident_id`. Set `expected_uid` to refuse an actor that replaced the one you
observed. For a running actor, also set `expected_activation` to the observed
run. Keep those conditions and the request ID unchanged on retries.

The caller needs `can_quarantine`: an editor or owner of the atespace, or the
`quarantine_manager` or `quarantine_admin` role. See
[authorization](authorization.md).

ateapi atomically places the hold, sets `status.quarantine` and stores a
receipt. A stale condition fails with `FAILED_PRECONDITION`. If the actor is
`RUNNING`, ateapi then suspends it under the stable child request ID formed by
appending `/suspend`. If suspension fails while the activation remains
assigned, termination fallback discards its memory and releases the worker.
The receipt records `QUARANTINE_STEP_SUSPENDED` or `QUARANTINE_STEP_TERMINATED`
when that step succeeds.

Termination fallback releases the actor before separately writing the final
receipt. An interruption between those writes can leave `terminate_pending`;
a retry then reports `ABORTED` with an unknown outcome. Current actor state
is not proof that the requested termination succeeded. Unfinished receipts
can advance; completed retries preserve their recorded result separately
from later observations.

## What the quarantine write refuses

- Resume, delete, revert, and creating a tag from the actor fail with
  `FAILED_PRECONDITION`, reason `TERMINAL_STATE`, and cause
  `ACTOR_QUARANTINED`. Ingress cannot resume the actor through these calls.
- `MintWorkerActorCertificate`, `MintActorJwt`, and the ordinary
  `GetActorEgressPolicy` call are refused. Existing certificates are not
  removed from clients.
- New atenet CONNECT tunnels are refused after a current actor read, even if
  the actor still reports `RUNNING`. Admission checks the signed actor UID
  and, when present, the signed activation before checking quarantine.
- Replaced snapshots remain in object storage rather than being collected.

An editor can still update the egress policy without releasing quarantine.
`UpdateActorEgressPolicy` needs its `uid` and `version`, obtained before
quarantine or through `InspectQuarantinedActorEgressPolicy` by a
`quarantine_admin`. Inspect grants no network access.

## Existing connections

Suspension or termination tears down the workload. atunnel closes its egress
tunnels, including TLS passthrough, and cancels in-flight ingress requests.

The downstream Envoy configuration also pairs an outer CONNECT guard with
network guards in every internal egress chain. They remain active during
listener inspection, incomplete TLS handshakes and established traffic.
The guards use an opaque per-CONNECT capability created from the verified
certificate, never an actor-supplied identity header. They poll through the
pod-local atenet listener and close on quarantine, UID replacement, failed or
overdue actor checks, or a mismatch with a certified activation.

A certificate without an activation remains bound only to its UID. Its
binding is never filled from current actor state, and activation rotation
alone does not revoke it. Ordinary holds do not revoke tunnels. Release can
admit a fresh CONNECT; it never reopens an old revoked capability.

Actor checks are shared per certified actor UID on each replica. A check has
a two-second deadline and a maximum five-second polling interval. Guards
fail closed when the seven-second freshness budget expires, including during
control-plane outages. The eight-second end-to-end closure budget is an
unqualified target under load. See the [Envoy module contract](../../cmd/dataplane/envoy/dynamic-modules/egress-policy/README.md#tunnel-revocation).
Closing a tunnel does not establish that idle pooled origin sockets have
closed or cancel work already accepted by an origin.

## Caches and lifecycle limits

atenet caches egress policies for ten seconds; atunnel caches DNS policy for
five seconds and retains its cached policy if refresh fails. Cached policy
does not extend a revoked tunnel's capability. Existing worker certificates
last an hour and renew at 90% of that lifetime. A refused renewal stops
atunnel's renewal loop and subsequent egress; gateway checks do not wait for
certificate expiry. Mint and renewal always carry the activation supplied by
Run or Restore, and cannot acquire a later activation's identity.

A `RESUMING` actor's finalization checks the hold and terminates the restored
workload. A `SUSPENDING` actor continues its existing suspend; retry that
operation with its own request ID if it is stuck. A `REVERTING` actor cannot
be quarantined until the revert finishes.

Conditional control-plane checks do not fence every worker effect. Worker
crash/suspend notifications and atelet Checkpoint/Terminate lack activation
identity. Delayed messages can affect a later activation on the same worker;
the suspend authorization-to-workflow race also remains. Certificate binding
and network revocation do not repair those lifecycle paths. Mixed patched
and unpatched writers and rollback are unqualified; unpatched writers can
discard quarantine/activation state or bypass restrictions.

## Evidence and release

The receipt's `snapshot_durable` describes durability at the recorded step,
not a later upload observation. Replaced snapshots stay under the actor's
object-store prefix until deletion after release. A `CreateTag` already
past its quarantine check can still finish its copy. There is no snapshot
export or isolated forensic restore API.

Each quarantine, completed suspend or terminate, release, and refused resume
writes `ate.actor.quarantine_changed` with caller, reason, incident ID,
request ID, UID, activation and steps. The configured log exporters determine
whether events appear locally, at an OTLP collector, or both. Envoy guards
emit `connect_revocation` with the frozen identity and reason, without the
capability. See the [event registry](../metrics/registry/events.yaml).

`ReleaseQuarantine` names the original quarantine request ID and requires
`quarantine_admin`. It does not resume the actor. The next routed request or
explicit resume can restore it under a new activation. After termination
fallback, that restore uses the last completed durable snapshot, or starts
from the template if none exists. Revert or delete after release if the
retained state must not run again. Owners can grant themselves the release
role, so restrict owner rights accordingly.

Quarantine does not cover a compromised node, worker or control plane,
template-wide or atespace-wide containment, or cancellation of upstream work
already forwarded. Gateway integration and deployed gVisor/microVM behavior
require qualification of the selected build and configuration.
