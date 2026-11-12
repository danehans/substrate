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
name=${0##*/}
root=$("$REAL_GIT" rev-parse --show-toplevel)
source_sha=$DRAFT_TEST_SOURCE
# shellcheck source=/dev/null
source "$root/hack/release/danehans/agentgateway.env"
gateway=$(sed -n 's/^  agentgateway: //p' "$root/charts/substrate/values.yaml")
digest=${gateway##*@}
printf '%s %s\n' "$name" "$*" >> "$RELEASE_TEST_LOG"
case "$name" in
 git)
  if [[ "$*" == 'rev-parse --verify refs/tags/'* || "$*" == 'rev-parse HEAD' ]]; then echo "$source_sha"; else exec "$REAL_GIT" "$@"; fi ;;
 docker)
  [[ "$DRAFT_TEST_MODE" != inspect-failure ]] || exit 1
  revision=$source_sha
  [[ "$4" != "$gateway" ]] || revision=$GATEWAY_REVISION
  [[ "$DRAFT_TEST_MODE" != wrong-revision ]] || revision=wrong
  arch=arm64
  [[ "$DRAFT_TEST_MODE" != missing-platform ]] || arch=386
  jq -n --arg digest "$digest" --arg revision "$revision" --arg arch "$arch" '{manifest:{digest:$digest,manifests:[{platform:{os:"linux",architecture:"amd64"}},{platform:{os:"linux",architecture:$arch}}]},image:{"linux/amd64":{config:{Labels:{"org.opencontainers.image.revision":$revision}}},"linux/arm64":{config:{Labels:{"org.opencontainers.image.revision":$revision}}}}}' ;;
 helm)
  case "$1 $2" in
   'pull '*)
    [[ "$DRAFT_TEST_MODE" != registry-failure ]] || exit 1
    [[ "$DRAFT_TEST_MODE" != missing-atelet || "$2" != */substrate-atelet ]] || exit 1
    echo "Digest: $digest" ;;
   'show chart') echo 'appVersion: v1.0.1-danehans.0' ;;
   'show values') echo "  agentgateway: $gateway" ;;
   'template '*)
    [[ "$DRAFT_TEST_MODE" != atelet-render-failure || "$2" != substrate-atelet ]] || exit 1
    if [[ "$DRAFT_TEST_MODE" == bad-chart-image ]]; then echo 'image: unpinned:latest'; else
     echo "image: $gateway"
     echo 'image: ghcr.io/danehans/substrate/atelet:v1.0.1-danehans.0'
    fi ;;
   *) exit 2 ;;
  esac ;;
 gh) echo 'unexpected release API call' >&2; exit 2 ;;
 *) exit 2 ;;
esac
