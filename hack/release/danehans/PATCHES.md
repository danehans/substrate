# danehans/substrate patches

`danehans/substrate` is an experimental, unqualified, development-only downstream of
`kagent-dev/substrate`. Every release is built from one commit on a
`danehans/<base tag>` branch, and each change below that is not in the base
names its upstream proposal and the condition that retires it.
[`fork.env`](fork.env) records the base.

## Functional patches

1. **Activation identity and lifecycle receipts.** Assignment, RESUMING,
   activation identity and the resume receipt commit together. Re-entering
   resume rotates the activation; clearing assignment clears it. Conditional
   lifecycle calls check the expected actor UID and activation and retain
   receipts under stable request IDs. Completed retries preserve their
   original evidence. See the [lifecycle contract](../../../docs/reference/api-guide.md#conditional-suspend-and-resume).
   - Upstream proposals:
     [activation identity](https://github.com/agent-substrate/substrate/issues/1041)
     and [conditional lifecycle](https://github.com/agent-substrate/substrate/issues/3065).
2. **Holds and quarantine.** Holder-scoped holds prevent resume. Quarantine
   atomically records its hold, state and receipt, then suspends a running
   activation or falls back to termination. Inspect grants quarantine admins
   access to stored native policy without granting network access. Termination
   fallback releases the actor before separately finalizing its receipt; an
   interruption can leave an unknown pending outcome. See the
   [quarantine contract](../../../docs/guides/actor-quarantine.md).
   - Upstream proposals:
     [actor holds](https://github.com/agent-substrate/substrate/issues/3066)
     and [quarantine](https://github.com/agent-substrate/substrate/issues/3463).
3. **Certificate and network binding.** Mint and renewal retain the activation
   supplied by Run or Restore. atenet CONNECT checks the verified certificate's
   UID and optional activation before quarantine. Paired Envoy guards continue
   those checks through listener inspection and established traffic. UID-only
   certificates remain UID-bound; current state never fills a missing
   activation. See the [Envoy contract](../../../cmd/dataplane/envoy/dynamic-modules/egress-policy/README.md#tunnel-revocation).
   - Upstream proposal:
     [authenticated activation identity](https://github.com/agent-substrate/substrate/issues/1041).

Retire these functional patches when a supported `kagent-dev/substrate`
release provides the corresponding contracts and Picket qualifies that runtime.
Worker crash/suspend notifications and atelet Checkpoint/Terminate still lack
activation identity. The suspend authorization race remains; these controls
do not guarantee end-to-end stale-message rejection. Mixed patched/unpatched
writers and rollback remain unqualified. Network closure does not establish
idle origin-socket retirement or cancel work already accepted upstream.

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
   `manifests/ate-install` keep the base gateway. The chart gateway returns
   `actor-quarantined`, `actor-uid-mismatch`, or `activation-mismatch` in the
   body of a refused CONNECT. Its
   `agentgateway::substrate::connect_refusal` log target records the reason
   code, certified actor identity and CONNECT authority at `warn`.
   - Upstream proposal:
     [agentgateway/agentgateway#4289](https://github.com/agentgateway/agentgateway/issues/4289)
     (expose the applied Substrate egress policy's UID and version to CEL and
     ext_proc), and
     [solo-io/picket#215](https://github.com/solo-io/picket/issues/215)
     (certificate and quarantine checks at CONNECT).
   - Replacement condition: an agentgateway release contains the change and
     a `kagent-dev/substrate` release bundles that gateway; the default then
     returns to the base chart's gateway.

## Gateway integration

The chart and `agentgateway.env` still name the historical source
`8400847e8046669145f287506661219d201a95a2` and its matching image index. That
pair does not contain the v1.0.1 actor-state alignment in the reviewed gateway
source `7696eaaf931945d8449012ebb4973d6bf2d4889a`. A fresh image of that source,
with verified platform manifests and provenance, is required before release.
The old digest must never be relabeled as that newer source. See the
[gateway source and artifact contract](https://github.com/danehans/agentgateway/pull/7#issuecomment-6831601211).

The chart selects `perExecution` upstream pooling by default; `shared` opts
into a shared pool. Integrated gateway behavior still requires validation of
the final source/image pair. The atenet/Envoy guards are present, but idle
origin-pool retirement remains a distinct limitation. Neither the eight-second
closure target under load nor the combined runtime is qualified.
