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
archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
curl -fLsS --retry 3 -H 'Accept: application/octet-stream' \
  -o "${archive}" \
  https://api.github.com/repos/kubernetes-sigs/controller-tools/releases/assets/458700685
echo "ea743186c8a799f5cf8faf16969f86189d003cb7d130e0ac4b58789f1e5748dcf30ebe91c837a10d5ac415383da3e10b9e64d65785c938c23e739781cfb76f08  ${archive}" | sha512sum -c -
hack/run-tool.sh setup-envtest sideload --os linux --arch amd64 1.36.2 < "${archive}"
