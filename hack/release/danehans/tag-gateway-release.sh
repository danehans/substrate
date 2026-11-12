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

set -euo pipefail
: "${VERSION:?}" "${KO_DOCKER_REPO:?}"
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
# shellcheck disable=SC1091 # The selected revision is sourced from the checkout.
source "$here/agentgateway.env"
"$here/check-tag.sh" "$VERSION"

image="$KO_DOCKER_REPO/agentgateway"
expected_prefix="$image:${GATEWAY_REVISION:0:12}@"
ref=$(sed -n 's/^  agentgateway: //p' "$root/charts/substrate/values.yaml")
if [[ "$ref" != "$expected_prefix"sha256:* || ! "$ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
  echo "chart gateway does not name the reviewed source and digest" >&2
  exit 1
fi
digest=${ref##*@}
if [[ $(crane digest "$image@$digest") != "$digest" ]]; then
  echo "chart gateway digest is unavailable" >&2
  exit 1
fi
existing=$("$here/registry-tag.sh" probe "$image" "$VERSION")
if [[ "$existing" == missing ]]; then
  crane tag "$image@$digest" "$VERSION"
elif [[ "$existing" != "$digest" ]]; then
  echo "gateway release tag already names a different digest" >&2
  exit 1
fi
if [[ $(crane digest "$image:$VERSION") != "$digest" ]]; then
  echo "gateway release tag does not match the chart digest" >&2
  exit 1
fi
