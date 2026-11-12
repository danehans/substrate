// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package release_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func releaseEnv(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"gh", "helm"} {
		if err := os.Symlink(filepath.Join(root, "hack/release/testdata", name+".sh"), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(bin, "helm.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELEASE_TEST_LOG", log)
	t.Setenv("RELEASE_TEST_STATE", "missing")
	t.Setenv("RELEASE_TEST_FAIL", "")
	t.Setenv("VERSION", "v1.0.0")
	t.Setenv("GITHUB_REPOSITORY", "kagent-dev/substrate")
	t.Setenv("GITHUB_ACTOR", "release-test")
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("KO_DOCKER_REPO", "ghcr.io/kagent-dev/substrate")
	return root, log
}

func TestPublishCharts(t *testing.T) {
	for _, tc := range []struct {
		name, state, fail, version string
		wantErr, wantPush          bool
	}{
		{name: "new release", state: "missing", version: "v1.0.0", wantPush: true},
		{name: "draft rerun", state: "draft", version: "v1.0.0", wantPush: true},
		{name: "prerelease", state: "missing", version: "v1.0.0-rc.1", wantPush: true},
		{name: "published", state: "published", version: "v1.0.0", wantErr: true},
		{name: "API failure", state: "unavailable", version: "v1.0.0", wantErr: true},
		{name: "package failure", state: "missing", fail: "package", version: "v1.0.0", wantErr: true},
		{name: "login failure", state: "missing", fail: "registry", version: "v1.0.0", wantErr: true},
		{name: "push failure", state: "missing", fail: "push", version: "v1.0.0", wantErr: true, wantPush: true},
		{name: "invalid version", state: "missing", version: "v1.0.0,latest", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, log := releaseEnv(t)
			t.Setenv("VERSION", tc.version)
			t.Setenv("RELEASE_TEST_STATE", tc.state)
			t.Setenv("RELEASE_TEST_FAIL", tc.fail)
			cmd := exec.Command("bash", filepath.Join(root, ".github/scripts/release/publish-charts.sh"))
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("publish charts: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			got := string(calls)
			if strings.Contains(got, "push ") != tc.wantPush {
				t.Fatalf("unexpected registry writes:\n%s", got)
			}
			if tc.state == "published" || tc.state == "unavailable" || tc.name == "invalid version" {
				if got != "" {
					t.Fatalf("helm ran before release validation:\n%s", got)
				}
			}
			if tc.wantErr {
				return
			}
			charts := []string{"substrate-crds", "substrate", "substrate-podcert", "substrate-atelet"}
			if strings.Count(got, "push ") != len(charts) || strings.Count(got, "package ") != len(charts) {
				t.Fatalf("incomplete chart set:\n%s", got)
			}
			for _, chart := range charts {
				want := chart + "-" + strings.TrimPrefix(tc.version, "v") + ".tgz oci://ghcr.io/kagent-dev/substrate/helm"
				if !strings.Contains(got, want) {
					t.Errorf("missing %s in:\n%s", want, got)
				}
			}
			if strings.Count(got, "--app-version "+tc.version) != len(charts) {
				t.Errorf("charts do not select the release images:\n%s", got)
			}
		})
	}
}

func TestDraftReleaseArtifactNotes(t *testing.T) {
	for _, tc := range []struct {
		repo, registry string
		artifacts      bool
	}{
		{repo: "agent-substrate/substrate", artifacts: true},
		{repo: "kagent-dev/substrate", registry: "ghcr.io/kagent-dev/substrate", artifacts: true},
		{repo: "kagent-dev/substrate", registry: "registry.example.com/releases", artifacts: true},
		{repo: "someone/substrate"},
	} {
		t.Run(tc.repo+"/"+tc.registry, func(t *testing.T) {
			root, _ := releaseEnv(t)
			t.Setenv("GITHUB_REPOSITORY", tc.repo)
			t.Setenv("KO_DOCKER_REPO", tc.registry)
			cmd := exec.Command("bash", filepath.Join(root, "hack/release/draft-release.sh"), "--dry-run", "v1.0.0")
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("draft notes: %v\n%s", err, out)
			}
			notes := string(out)
			if strings.Contains(notes, "## Images") != tc.artifacts || strings.Contains(notes, "## kubectl-ate") != tc.artifacts {
				t.Fatalf("incorrect artifact notes:\n%s", notes)
			}
			if strings.Contains(notes, "## Helm charts") != (tc.repo == "kagent-dev/substrate") {
				t.Fatalf("incorrect chart notes:\n%s", notes)
			}
			if tc.artifacts {
				repo := tc.registry
				if repo == "" {
					repo = "ghcr.io/" + tc.repo
				}
				if !strings.Contains(notes, "--image-repo "+repo+" --image-tag v1.0.0") {
					t.Errorf("incorrect install image repository:\n%s", notes)
				}
			}
		})
	}
}

func TestDanehansCheckTag(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for tag, ok := range map[string]bool{
		"v1.0.1-danehans.0":        true,
		"v1.0.1-danehans.12":       true,
		"v1.0.1":                   false,
		"v1.0.0":                   false,
		"v1.0.1-danehans.01":       false,
		"v1.0.1-danehans.":         false,
		"v1.0.1-danehans.0x":       false,
		"v1X0X1-danehans.0":        false,
		"v1.0.0-rc1-danehans.0":    false,
		"v1.0.1-danehans.0,latest": false,
	} {
		out, err := exec.Command("bash", filepath.Join(root, "hack/release/danehans/check-tag.sh"), tag).CombinedOutput()
		if (err == nil) != ok {
			t.Errorf("check-tag %q: err=%v, want ok=%v\n%s", tag, err, ok, out)
		}
	}
}

func TestDanehansPublishCharts(t *testing.T) {
	for _, tc := range []struct {
		name, tag    string
		dry, wantErr bool
	}{
		{name: "valid", tag: "v1.0.1-danehans.0"},
		{name: "dry", tag: "v1.0.1-danehans.12", dry: true},
		{name: "upstream", tag: "v1.0.1", wantErr: true},
		{name: "old base", tag: "v1.0.0-rc1-danehans.0", wantErr: true},
		{name: "invalid dry", tag: "v1.0.1-danehans.01", dry: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, log := releaseEnv(t)
			t.Setenv("GITHUB_REPOSITORY", "danehans/substrate")
			t.Setenv("KO_DOCKER_REPO", "ghcr.io/danehans/substrate")
			t.Setenv("VERSION", tc.tag)
			args := []string{filepath.Join(root, ".github/scripts/release/publish-charts.sh")}
			if tc.dry {
				args = append(args, "--dry-run")
				t.Setenv("GH_TOKEN", "")
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("publish: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if (tc.dry || tc.wantErr) && (strings.Contains(string(calls), "push ") || strings.Contains(string(calls), "registry ")) {
				t.Fatalf("unexpected writes: %s", calls)
			}
			if !tc.wantErr && strings.Count(string(calls), "package ") != 4 {
				t.Fatalf("incomplete chart set: %s", calls)
			}
		})
	}
}

func TestDanehansDraft(t *testing.T) {
	for _, mode := range []string{"valid", "missing-platform", "wrong-revision", "inspect-failure", "bad-checksum", "missing-binary", "missing-atelet", "atelet-render-failure", "bad-chart-image", "registry-failure"} {
		t.Run(mode, func(t *testing.T) {
			root, log := releaseEnv(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			for _, name := range []string{"git", "helm", "docker", "gh"} {
				if err := os.Symlink(filepath.Join(root, "hack/release/testdata/danehans-tool.sh"), filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			source, err := exec.Command(realGit, "-C", root, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("DRAFT_TEST_SOURCE", strings.TrimSpace(string(source)))
			t.Setenv("REAL_GIT", realGit)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DRAFT_TEST_MODE", mode)
			t.Setenv("GITHUB_REPOSITORY", "danehans/substrate")
			t.Setenv("KO_DOCKER_REPO", "ghcr.io/danehans/substrate")
			assets := t.TempDir()
			for _, platform := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
				name := "kubectl-ate-" + platform
				data := []byte(name)
				if err := os.WriteFile(filepath.Join(assets, name), data, 0600); err != nil {
					t.Fatal(err)
				}
				sum := fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
				if err := os.WriteFile(filepath.Join(assets, name+".sha256"), []byte(sum), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "bad-checksum" {
				if err := os.WriteFile(filepath.Join(assets, "kubectl-ate-linux-amd64"), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-binary" {
				if err := os.Remove(filepath.Join(assets, "kubectl-ate-darwin-arm64")); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(root, "hack/release/danehans/draft-release.sh"), "--dry-run", "v1.0.1-danehans.0", assets)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if (err == nil) != (mode == "valid") {
				t.Fatalf("draft: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "gh ") {
				t.Fatalf("dry run called release API: %s", calls)
			}
			if mode == "valid" {
				for _, want := range []string{"substrate-atelet", "envoy-dataplane", "v1.0.1", "Dry run: no release created."} {
					if !strings.Contains(string(out), want) {
						t.Errorf("notes missing %q", want)
					}
				}
			}
		})
	}
}

func TestGatewayBuildTags(t *testing.T) {
	root, _ := releaseEnv(t)
	for tag, ok := range map[string]bool{"8400847e8046": true, "review-test": true, "v1.0.1-danehans.0": false, "v1.0.0-rc1-danehans.5": false, "v1.0.1-danehans.01": false, "bad/tag": false, "": false} {
		out, err := exec.Command("bash", filepath.Join(root, "hack/release/danehans/check-gateway-tag.sh"), tag).CombinedOutput()
		if (err == nil) != ok {
			t.Errorf("tag %q: %v %s", tag, err, out)
		}
	}
}
