# danehans/substrate patches

`danehans/substrate` is an experimental, unqualified downstream of
`kagent-dev/substrate`. Every release is built from one commit on a
`danehans/<base tag>` branch, and each change below that is not in the base
names its upstream proposal and the condition that retires it.
[`fork.env`](fork.env) records the base.

## Functional patches

None. Substrate's API, controllers, node agent and workers are built from the
base source unchanged.

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
