// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package images

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDownstreamReleaseInventory(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(root, "hack/release/danehans/list-release-images.sh"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release inventory: %v\n%s", err, out)
	}
	var want []string
	for _, pkg := range ReleaseComponents() {
		want = append(want, ImageName(pkg))
	}
	for _, img := range ReleaseDockerfileImages() {
		want = append(want, img.Name)
	}
	got := strings.Fields(string(out))
	if !slices.Equal(got, want) {
		t.Fatalf("release verification images = %v, publisher = %v", got, want)
	}
}
