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

# Refuses any tag that is not a danehans/substrate release of this branch's
# base: <BASE_TAG>-danehans.<N>, with N a non-negative integer without
# leading zeros. The release workflow runs it before publishing anything in
# danehans/substrate, so an upstream tag pushed to the fork publishes nothing.
#
#   hack/release/danehans/check-tag.sh <tag>

set -o errexit -o nounset -o pipefail

[[ $# -eq 1 ]] || {
  echo "usage: $0 <tag>" >&2
  exit 2
}
# shellcheck source=fork.env
source "$(dirname "${BASH_SOURCE[0]}")/fork.env"
tag="$1"
base="${BASE_TAG//./\\.}"
if [[ ! "${tag}" =~ ^${base}-danehans\.(0|[1-9][0-9]*)$ ]]; then
  echo "error: ${tag} is not a ${FORK_REPOSITORY} release tag (${BASE_TAG}-danehans.N)" >&2
  exit 1
fi
