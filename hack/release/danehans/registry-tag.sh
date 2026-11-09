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

if [[ $# != 3 || $1 != probe ]]; then
  echo "usage: $0 probe IMAGE TAG" >&2
  exit 2
fi

error=$(mktemp)
trap 'rm -f "$error"' EXIT
if digest=$(crane digest "$2:$3" 2>"$error"); then
  printf '%s\n' "$digest"
elif grep -q 'MANIFEST_UNKNOWN' "$error"; then
  printf 'missing\n'
else
  cat "$error" >&2
  exit 1
fi
