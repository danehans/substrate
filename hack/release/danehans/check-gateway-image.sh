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
: "${REGISTRY_IMAGE:?}" "${IMAGE_TAG:?}" "${GITHUB_OUTPUT:?}" "${GITHUB_STEP_SUMMARY:?}"
here=$(cd "$(dirname "$0")" && pwd)
digest=$("$here/registry-tag.sh" probe "$REGISTRY_IMAGE" "$IMAGE_TAG")
if [[ "$digest" == missing ]]; then
  printf 'exists=false\n' >> "$GITHUB_OUTPUT"
else
  printf 'exists=true\n' >> "$GITHUB_OUTPUT"
  # shellcheck disable=SC2016 # Markdown backticks are literal.
  printf 'Existing image: `%s:%s@%s`\n' "$REGISTRY_IMAGE" "$IMAGE_TAG" "$digest" >> "$GITHUB_STEP_SUMMARY"
fi
