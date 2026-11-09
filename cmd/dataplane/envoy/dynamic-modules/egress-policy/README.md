# Envoy Substrate Egress Policy Implementation

This Envoy dynamic module stores and enforces egress policy and checks ongoing
exact-run tunnel authorization. Its policy extensions minimize
memory usage by Substrate egress policies.

To minimize memory usage egress policies are hashed using [Rapidhash](https://docs.rs/rapidhash/latest/rapidhash/)
which produces 64 bit hashes with low probability of collision. Policies are then stored in the worker local
store using computed hash and the hash is exchanged between outer CONNECT request and inner
listener that terminates TCP connection encapsulated in the CONNECT request.

## Details

In the outer CONNECT filter chain the policy store HTTP filter is instantiated after
the ext_proc filter. After ext_proc successfully looks up egress policy for the given actor and
destination port, the subsequent policy store filter:

1. Creates EgressPolicy object by parsing JSON from metadata sent by the ext_proc server and computes its hash.
1. Checks if the policy with this hash already exists in the worker local policy store.
1. If it does it adds a reference to the existing policy.
1. Otherwise it stores the new policy in the store.
1. The policy hash is written to filter state, which the chain's set_filter_state filter copies into filter state shared with the upstream.

In the inner listener filter chain, the policy enforcement listener filter:

1. reads policy hash from the filter state, shared by the outer CONNECT filter chain.
1. Looks up the policy object by hash in the worker local policy store.
1. Evaluates and enforces egress policy.

With this approach, egress policy objects are shared by many CONNECT requests. Per request state is
a 64 bit hash. For example if all actors have the same policy, Envoy will keep only one policy object in memory
and all requests will share the same object.

## Policy Lifetime

Ownership of EgressPolicy objects is shared by policy store HTTP filters and the worker local policy store. As such
policy object is guaranteed to outlive all CONNECT requests that share the same policy. Lifetime of the CONNECT request
is at least as long as the lifetime of the inner request, so it is guaranteed that inner request will always
find a policy with hash it received from the outer CONNECT request.

To avoid leaking policy object in the local policy store, policy store HTTP filter, during its cleanup, checks the
reference count value of the EgressPolicy. If this count value is 2, then only this filter and policy store have outstanding
references, and at this point it is safe to remove policy object from the store. This guarantees that policy is destroyed,
when no more CONNECT requests are referencing it.

## Policy Hashing

[Rapidhash](https://docs.rs/rapidhash/latest/rapidhash/) is considered one of the best non-cryptographic hashing functions
with near [ideal collision probability](https://chromium.googlesource.com/chromium/src/+/HEAD/third_party/rapidhash/README.md).
Other options are xxhash which can produce 128 bit hashes or cryptographic hash such as SHA-256. 64 bit Rapidhash offers the
best balance between quality and performance, with approximately 1 in 10^19 probability of collision.

## Envoy stats

The HTTP filter (`envoy_substrate_egress_policy_store`) defines the following
metrics:

| Metric | Type | Description |
|---|---|---|
| `ate_egress.policy_store_size` | Gauge | Current number of unique `EgressPolicy` entries stored in the worker thread's local store (summed across workers by Envoy). |
| `ate_egress.parse_errors` | Counter | Number of times `EgressPolicy::new_from_metadata` returned `None` due to missing or malformed policy metadata. |
| `ate_egress.policy_shared` | Counter | Number of times a `CONNECT` request reused an `EgressPolicy` already present in the worker thread's local store. |

## Exact-run tunnel revocation

The downstream fork guards the outer CONNECT stream with
`envoy_substrate_revocation_http`, after its trusted filter state is set. This
guard remains active while TLS inspection or an incomplete ClientHello waits.
It also installs `envoy_substrate_revocation` before the HTTP
connection manager and passthrough TCP proxy in every internal egress chain.
The CONNECT ext_proc handler derives its binding from the verified workload
certificate and issues an opaque per-CONNECT capability. Envoy carries it in
shared filter state; it never comes from actor request headers. A separate
trusted `legacy` marker preserves CONNECT behavior for certificates without an
activation. Missing required state closes the connection.

The network filter checks its capability against the loopback-only atenet
control listener on port 18081. The sidecar polls authenticated `GetActor`
once per active certificate-bound execution per replica, with jitter, a
maximum five-second interval and a two-second RPC deadline. A UID or activation
mismatch takes precedence over quarantine. Quarantine, deletion, lookup failure
or stale scheduling invalidates existing handles. An ordinary hold does not.
Release permits a fresh admitted CONNECT; it never reopens old handles.

A monotonic seven-second freshness watchdog closes MITM HTTP/2 streams and
passthrough copies. Local capability checks run every 500 ms; the scheduler
runs every 100 ms. The outer CONNECT HCM disables delayed close. The eight-second
end-to-end budget remains an unqualified target under runtime scheduling and
control-plane load. In-flight upstream work may outlive socket closure.
Control-plane outages can close permitted connections. Pending or abandoned
capabilities expire after ten seconds without a local heartbeat.

The module logs `connect_revocation` with its reason and frozen UID, activation
and authority, without logging capabilities. `ate_egress.revocation.closed`
counts local network close requests; `ate_egress.revocation.stream_closed`
counts outer CONNECT resets. Both phases share one capability and actor poll
group. Retiring one phase does not retire the other phase. The sidecar exports `atenet.router.revocation.*`
actor/tunnel gauges and lookup/failure/overdue counters without identity labels.
These mechanisms do not qualify a Picket network effect or change its pins.

Run the Envoy integration harness explicitly after building the local image:

```sh
docker build -t q6-atenet-envoy cmd/dataplane/envoy
Q6_ENVOY_IMAGE=q6-atenet-envoy:latest go test -tags envoyintegration ./cmd/atenet/internal/router/egress -run TestEnvoyRevokesEstablishedConnections -count=3
```

The fixture uses real Envoy connections and a synthetic authenticated actor
view. It covers TLS passthrough and TLS-terminated pooled HTTP/2 without worker
teardown; it does not qualify a deployed ateapi, independent gateway replicas
or a Picket processor response.
