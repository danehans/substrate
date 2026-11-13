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

# Execute the published gateway in the installed pods on their native nodes.
set -o errexit -o nounset -o pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}"
source hack/release/danehans/agentgateway.env

image="$(awk '$1 == "agentgateway:" && NF == 2 { print $2 }' charts/substrate/values.yaml)"
if [[ ! "${image}" =~ @sha256:[0-9a-f]{64}$ ]]; then
  echo "The chart gateway must name one immutable image." >&2
  exit 1
fi
for deployment in atenet-router atenet-egress; do
  selector="$(kubectl -n ate-system get deployment "${deployment}" -o json | jq -er '
    .spec.selector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")
    | select(length > 0)')"
  pods="$(kubectl -n ate-system get pods -l "${selector}" -o json | jq -er '
    if (.items | length) == 0 then error("gateway deployment has no pods")
    else .items[].metadata.name end')"
  while IFS= read -r pod; do
    record="$(kubectl -n ate-system get pod "${pod}" -o json)"
    jq -e --arg image "${image}" '
      any(.spec.containers[]; .name == "agentgateway" and .image == $image)
      and any(.status.containerStatuses[]; .name == "agentgateway" and .ready)
    ' <<<"${record}" >/dev/null
    node="$(jq -er '.spec.nodeName' <<<"${record}")"
    platform="$(kubectl get node "${node}" -o json | jq -er '
      .status.nodeInfo | .operatingSystem + "/" + .architecture')"
    case "${platform}" in
      linux/amd64) target=x86_64-unknown-linux-gnu ;;
      linux/arm64) target=aarch64-unknown-linux-gnu ;;
      *) echo "Unsupported gateway test node: ${platform}" >&2; exit 1 ;;
    esac
    version="$(kubectl -n ate-system exec "${pod}" -c agentgateway -- /app/agentgateway --version)"
    jq -e --arg revision "${GATEWAY_REVISION}" --arg target "${target}" '
      .git_revision == $revision and .build_target == $target
    ' <<<"${version}" >/dev/null
    printf 'Pod: %s; node: %s; platform: %s\n' "${pod}" "${node}" "${platform}"
    jq -r '.status.containerStatuses[] | select(.name == "agentgateway") | .imageID' <<<"${record}"
    printf '%s\n' "${version}"
  done <<<"${pods}"
done
