#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Creates or refreshes the DRAFT release for a danehans/substrate tag. The
# notes record the source and base revisions, the patch list (PATCHES.md), and
# the digest of every published artifact, read back from the registry:
#
# - every release image, which must carry linux/amd64 and linux/arm64 and be
#   labeled with the tag's commit on every platform;
# - the four Helm charts, which must select this registry and the tag;
# - every image the charts render by default; third-party images must be
#   pinned by digest, and the gateway must be built from agentgateway.env;
# - the kubectl-ate checksums in <binaries dir>.
#
#   hack/release/danehans/draft-release.sh [--dry-run] <tag> <binaries dir>
#
# --dry-run prints the notes and creates nothing. A published release is never
# changed. Needs git, jq, helm, docker buildx, read access to the registry and
# an authenticated gh with write access to GITHUB_REPOSITORY.

set -o errexit -o nounset -o pipefail

usage() {
  echo "usage: $0 [--dry-run] <tag> <binaries dir>" >&2
  exit 2
}

dry_run=false
if [[ "${1:-}" == --dry-run ]]; then
  dry_run=true
  shift
fi
[[ $# -eq 2 ]] || usage
tag="$1"
bin_dir="$2"

ROOT="$(git rev-parse --show-toplevel)"
HERE="${ROOT}/hack/release/danehans"
# shellcheck source=hack/release/danehans/fork.env
source "${HERE}/fork.env"
# shellcheck source=hack/release/danehans/agentgateway.env
source "${HERE}/agentgateway.env"
: "${GITHUB_REPOSITORY:?}" "${KO_DOCKER_REPO:?}"
if [[ "${GITHUB_REPOSITORY}" != "${FORK_REPOSITORY}" ]]; then
  echo "error: this script drafts ${FORK_REPOSITORY} releases, not ${GITHUB_REPOSITORY}" >&2
  exit 1
fi
"${HERE}/check-tag.sh" "${tag}"
registry="${KO_DOCKER_REPO%/}"
if [[ "${registry}" != "ghcr.io/${FORK_REPOSITORY}" ]]; then
  echo "error: unexpected downstream registry ${registry}" >&2
  exit 1
fi
version="${tag#v}"

source_sha="$(git rev-parse --verify "refs/tags/${tag}^{commit}")"
if [[ "$(git rev-parse HEAD)" != "${source_sha}" ]]; then
  echo "error: checkout does not match ${tag}" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "${BASE_REVISION}" "${source_sha}"; then
  echo "error: ${tag} (${source_sha}) does not descend from ${BASE_REPOSITORY}@${BASE_REVISION}" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# inspect <ref> prints "<digest> <platforms>" for an image index and fails
# unless it carries linux/amd64 and linux/arm64. With a revision, every
# platform's org.opencontainers.image.revision label must equal it.
inspect() {
  local ref="$1" revision="${2:-}" json
  json="$(docker buildx imagetools inspect "${ref}" --format '{{json .}}')"
  local digest platforms
  digest="$(jq -er '.manifest.digest' <<<"${json}")"
  platforms="$(jq -r '[.manifest.manifests[]?.platform | select(.os != "unknown") | "\(.os)/\(.architecture)"] | sort | join(", ")' <<<"${json}")"
  if [[ "${platforms}" != *linux/amd64* || "${platforms}" != *linux/arm64* ]]; then
    echo "error: ${ref} is not published for linux/amd64 and linux/arm64 (${platforms:-single manifest})" >&2
    return 1
  fi
  if [[ -n "${revision}" ]]; then
    local revisions
    revisions="$(jq -r '[.image[] | .config.Labels["org.opencontainers.image.revision"] // "none"] | unique | join(",")' <<<"${json}")"
    if [[ "${revisions}" != "${revision}" ]]; then
      echo "error: ${ref} is labeled with revision ${revisions}, not ${revision}" >&2
      return 1
    fi
  fi
  echo "${digest} ${platforms}"
}

# Read the same allowlists ate-setup publishes. The images package tests this
# script against ReleaseComponents and ReleaseDockerfileImages.
image_names="$("${HERE}/list-release-images.sh")"
release_images=()
while IFS= read -r name; do
  [[ -n "${name}" ]] && release_images+=("${name}")
done <<<"${image_names}"
[[ ${#release_images[@]} -gt 0 ]] || { echo "error: empty release image set" >&2; exit 1; }

image_rows=""
for name in "${release_images[@]}"; do
  ref="${registry}/${name}:${tag}"
  inspection="$(inspect "${ref}" "${source_sha}")"
  read -r digest platforms <<<"${inspection}"
  image_rows+="| \`${name}\` | \`${ref}@${digest}\` | ${platforms} |"$'\n'
done

helm_out="$(mktemp -d "${work}/helm.XXXX")"
chart_rows=""
for chart in substrate-crds substrate substrate-podcert substrate-atelet; do
  out="$(helm pull "oci://${registry}/helm/${chart}" --version "${version}" --destination "${helm_out}" 2>&1)"
  digest="$(sed -n 's/^Digest: \(sha256:[0-9a-f]\{64\}\)$/\1/p' <<<"${out}")"
  if [[ -z "${digest}" ]]; then
    echo "error: no digest for ${chart} ${version}: ${out}" >&2
    exit 1
  fi
  app_version="$(helm show chart "${helm_out}/${chart}-${version}.tgz" | sed -n 's/^appVersion: //p' | tr -d '"')"
  if [[ "${app_version}" != "${tag}" ]]; then
    echo "error: ${chart} ${version} has appVersion ${app_version}, not ${tag}" >&2
    exit 1
  fi
  chart_rows+="| \`${chart}\` | \`${registry}/helm/${chart}:${version}@${digest}\` |"$'\n'
done

# Images the charts deploy by default. Release images are listed above.
rendered="$(
  {
    helm template substrate "${helm_out}/substrate-${version}.tgz" --namespace ate-system || exit 1
    helm template substrate-atelet "${helm_out}/substrate-atelet-${version}.tgz" --namespace ate-system || exit 1
    helm template substrate-podcert "${helm_out}/substrate-podcert-${version}.tgz" --namespace certificate-system || exit 1
  } | sed -n 's/^[[:space:]]*-\{0,1\}[[:space:]]*image:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}[[:space:]]*$/\1/p' | sort -u
)"
gateway_ref="$(helm show values "${helm_out}/substrate-${version}.tgz" | sed -n 's/^  agentgateway: //p')"
if ! grep -qxF "${gateway_ref}" <<<"${rendered}"; then
  echo "error: the substrate chart does not render its gateway ${gateway_ref:-(unset)}" >&2
  exit 1
fi
third_party_rows=""
while IFS= read -r ref; do
  [[ -n "${ref}" ]] || continue
  # The gateway may live under the release registry; it is checked below
  # against agentgateway.env rather than as a release image.
  [[ "${ref}" == "${gateway_ref}" ]] && continue
  if [[ "${ref}" == "${registry}/"* ]]; then
    name="${ref#"${registry}/"}"
    name="${name%%:*}"
    if [[ "${ref}" != "${registry}/${name}:${tag}" ]] || ! printf '%s\n' "${release_images[@]}" | grep -qxF "${name}"; then
      echo "error: the charts render ${ref}, which is not a release image of ${tag}" >&2
      exit 1
    fi
    continue
  fi
  if [[ ! "${ref}" =~ @sha256:[0-9a-f]{64}$ ]]; then
    echo "error: the charts render ${ref}, which is not pinned by digest" >&2
    exit 1
  fi
  third_party_rows+="| \`${ref}\` |"$'\n'
done <<<"${rendered}"
inspection="$(inspect "${gateway_ref}" "${GATEWAY_REVISION}")"
read -r gateway_index gateway_platforms <<<"${inspection}"
if [[ "${gateway_ref}" != *"@${gateway_index}" ]]; then
  echo "error: the gateway default ${gateway_ref} does not name its index digest ${gateway_index}" >&2
  exit 1
fi

checksums=""
for os in linux darwin; do
  for arch in amd64 arm64; do
    name="kubectl-ate-${os}-${arch}"
    expected="$(cat "${bin_dir}/${name}.sha256")"
    actual="$(cd "${bin_dir}" && shasum -a 256 "${name}")"
    if [[ "${actual}" != "${expected}" ]]; then
      echo "error: checksum mismatch for ${name}" >&2
      exit 1
    fi
    checksums+="${actual}"$'\n'
  done
done

# The patch list from PATCHES.md, from its first section on, with headings
# demoted and its links to files beside it made absolute at the source revision.
patches="$(awk '/^## Functional patches/ {f = 1} f' "${HERE}/PATCHES.md" |
  sed -e 's/^## /### /' \
    -e "s#](\\([a-z.]*\\.env\\))#](https://github.com/${FORK_REPOSITORY}/blob/${source_sha}/hack/release/danehans/\\1)#g")"
if [[ -z "${patches}" ]]; then
  echo "error: ${HERE}/PATCHES.md has no patch list" >&2
  exit 1
fi

notes="$(cat <<NOTES
**Experimental, unqualified, and development only.** \`${FORK_REPOSITORY}\` is a downstream of \`${BASE_REPOSITORY}\` \`${BASE_TAG}\`. It is not an upstream release, and no protection or qualification is claimed for it.

## Source

| | Repository | Revision |
| --- | --- | --- |
| Release source | [\`${FORK_REPOSITORY}\`](https://github.com/${FORK_REPOSITORY}/tree/${source_sha}) | \`${source_sha}\` |
| Base | [\`${BASE_REPOSITORY}\` \`${BASE_TAG}\`](https://github.com/${BASE_REPOSITORY}/tree/${BASE_REVISION}) | \`${BASE_REVISION}\` |
| Egress gateway | [\`${GATEWAY_REPOSITORY}\`](https://github.com/${GATEWAY_REPOSITORY}/tree/${GATEWAY_REVISION}) | \`${GATEWAY_REVISION}\` |

Every release image below carries the release source revision label.
Chart versions and rendered image references were checked against the release tag.

## Patches

${patches}

## Release images

| Image | Reference | Platforms |
| --- | --- | --- |
${image_rows}
## Helm charts

| Chart | Reference |
| --- | --- |
${chart_rows}
## Egress gateway

| Reference | Platforms |
| --- | --- |
| \`${gateway_ref}\` | ${gateway_platforms} |

## Third-party images rendered by the charts

| Reference |
| --- |
${third_party_rows}
## kubectl-ate

\`\`\`text
${checksums}
\`\`\`

Download the binary for your platform and its \`.sha256\` file, then check it:

\`\`\`sh
shasum -a 256 -c kubectl-ate-linux-amd64.sha256
\`\`\`
NOTES
)"

echo "Release notes for ${tag} (${source_sha}):"
echo
echo "${notes}"
echo

if [[ "${dry_run}" == true ]]; then
  echo "Dry run: no release created."
  exit 0
fi
VERSION="${tag}" "${ROOT}/.github/scripts/release/refuse-published-release.sh"
if is_draft="$(gh release view "${tag}" --repo "${GITHUB_REPOSITORY}" --json isDraft --jq .isDraft 2>/dev/null)"; then
  if [[ "${is_draft}" != true ]]; then
    echo "${tag} is published; its notes are left unchanged."
    exit 0
  fi
  gh release edit "${tag}" --repo "${GITHUB_REPOSITORY}" --notes-file - <<<"${notes}"
  exit 0
fi
gh release create "${tag}" --repo "${GITHUB_REPOSITORY}" --draft --prerelease --verify-tag \
  --title "${tag}" --notes-file - <<<"${notes}"
