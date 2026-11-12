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
root=$(git rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat > "$work/bin/crane" <<'CRANE'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  digest)
    case "$2" in
      *@sha256:*) printf '%s\n' "${2##*@}" ;;
      *:v1.0.1-danehans.999)
        if [[ -f "$MOCK_RELEASE_DIGEST" ]]; then
          cat "$MOCK_RELEASE_DIGEST"
        else
          echo 'MANIFEST_UNKNOWN' >&2
          exit 1
        fi ;;
      *:8400847e8046)
        if [[ -f "$MOCK_SHORT_DIGEST" ]]; then
          cat "$MOCK_SHORT_DIGEST"
        else
          echo 'MANIFEST_UNKNOWN' >&2
          exit 1
        fi ;;
      *) echo 'unexpected crane digest argument' >&2; exit 1 ;;
    esac ;;
  tag)
    printf '%s\n' "$2 $3" >> "$MOCK_TAG_CALLS"
    printf '%s\n' "${2##*@}" > "$MOCK_RELEASE_DIGEST" ;;
  *) echo 'unexpected crane command' >&2; exit 1 ;;
esac
CRANE
chmod +x "$work/bin/crane"
export PATH="$work/bin:$PATH"
export MOCK_RELEASE_DIGEST="$work/release-digest"
export MOCK_SHORT_DIGEST="$work/short-digest"
export MOCK_TAG_CALLS="$work/tag-calls"
export VERSION=v1.0.1-danehans.999
export KO_DOCKER_REPO=ghcr.io/danehans/substrate
export REGISTRY_IMAGE="$KO_DOCKER_REPO/agentgateway"
export IMAGE_TAG=8400847e8046
export GITHUB_OUTPUT="$work/output"
export GITHUB_STEP_SUMMARY="$work/summary"
digest=$(sed -n 's/^  agentgateway: .*@//p' "$root/charts/substrate/values.yaml")

# An unchanged gateway revision is skipped on repeated workflow runs.
printf '%s\n' "$digest" > "$MOCK_SHORT_DIGEST"
"$root/hack/release/danehans/check-gateway-image.sh"
"$root/hack/release/danehans/check-gateway-image.sh"
[[ $(cat "$GITHUB_OUTPUT") == $'exists=true\nexists=true' ]]
[[ $(cat "$MOCK_SHORT_DIGEST") == "$digest" ]]
[[ ! -e "$MOCK_TAG_CALLS" ]]

# A release tag is created once from the chart's digest and never overwritten.
"$root/hack/release/danehans/tag-gateway-release.sh"
"$root/hack/release/danehans/tag-gateway-release.sh"
[[ $(wc -l < "$MOCK_TAG_CALLS") -eq 1 ]]
[[ $(cat "$MOCK_RELEASE_DIGEST") == "$digest" ]]
printf '%s\n' 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' > "$MOCK_RELEASE_DIGEST"
if "$root/hack/release/danehans/tag-gateway-release.sh" > "$work/failure" 2>&1; then
  echo 'mismatched release tag was overwritten' >&2
  exit 1
fi
[[ $(wc -l < "$MOCK_TAG_CALLS") -eq 1 ]]

rm "$MOCK_SHORT_DIGEST"
"$root/hack/release/danehans/check-gateway-image.sh"
[[ $(tail -n 1 "$GITHUB_OUTPUT") == 'exists=false' ]]
