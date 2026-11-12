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

# Print the image names in ate-setup's release allowlists.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
awk '
  /^var released = \[\]string\{/ { components = 1; next }
  components && /^}/ { components = 0 }
  components { sub(/\/\/.*/, ""); gsub(/[",[:space:]]/, ""); sub(/^.*\//, ""); if ($0 != "") print }
  /^var [A-Za-z]+ = DockerfileImage\{/ { declaration = $2 }
  declaration && /Name: "/ {
    name = $0; sub(/^.*Name: "/, "", name); sub(/".*/, "", name)
    names[declaration] = name; declaration = ""
  }
  /^var releasedDockerfiles = / { dockerfiles = 1; sub(/^.*\{/, "") }
  dockerfiles {
    sub(/\/\/.*/, "")
    if (/}/) { dockerfiles = 0; sub(/}.*/, "") }
    gsub(/[[:space:]]/, "")
    n = split($0, selected, ",")
    for (i = 1; i <= n; i++) {
      if (selected[i] == "") continue
      if (!(selected[i] in names)) { print "unknown release Dockerfile image" > "/dev/stderr"; exit 1 }
      print names[selected[i]]
    }
  }
' "$root/cmd/ate-setup/internal/images/images.go"
