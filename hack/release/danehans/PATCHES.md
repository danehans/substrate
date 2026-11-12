# danehans/substrate patches

`danehans/substrate` is an experimental, unqualified, development-only downstream of
`kagent-dev/substrate`. Every release is built from one commit on a
`danehans/<base tag>` branch, and each change below that is not in the base
names its upstream proposal and the condition that retires it.
[`fork.env`](fork.env) records the base.

## Functional patches

This packaging-only branch does not include the activation ID, certificate
binding, lifecycle receipt, actor hold, or quarantine changes. Their ports
and the selected gateway require integration and validation before release.
The integration owner must replace this section with the integrated patch
contracts and upstream replacement conditions before preparing a release.

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

The selected gateway source contains continuing CONNECT authorization and
execution-scoped upstream pools. The chart selects `perExecution` by default;
`shared` opts into a shared upstream pool. Its revocation behavior depends on
the integrated actor identity and quarantine contracts. Validate those
contracts with the gateway before release. No runtime qualification is claimed.

The atenet/Envoy revocation patches and their documentation belong to the
separate runtime integration. They are not present in this packaging branch.
