package main

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/releaseverify"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func releaseBuildTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, path := range []string{"internal/workflowpolicy", "hack", "bin", "cmd/oberth", "cmd/oberth-source-plugin", "charts/oberth", "base-image", ".oberth/pins"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"cmd/oberth/main.go", "cmd/oberth-source-plugin/main.go", "charts/oberth/Chart.yaml", "base-image/index.json", ".oberth/pins/base-image.digest"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "../../internal/workflowpolicy/release-version.pattern", filepath.Join(dir, "internal/workflowpolicy/release-version.pattern"))
	copyFile(t, "../../hack/release-bytes.sh", filepath.Join(dir, "hack/release-bytes.sh"))
	copyFile(t, "../../go.mod", filepath.Join(dir, "go.mod"))
	copyFile(t, "../../.oberth/pins/go-toolchain", filepath.Join(dir, ".oberth/pins/go-toolchain"))
	writeFixtureCommand(t, filepath.Join(dir, "bin/git"), "case \"$*\" in *--format=%cI*) printf '%s\\n' \"${COMMIT_DATE:-2026-11-17T12:34:56Z}\";; *status*) printf '%s' \"${DIRTY:-}\";; *) printf '%040d\\n' 0;; esac\n")
	source := `case "$*" in
  'env GOVERSION') printf '%s\n' "${TOOLCHAIN:-$(cat .oberth/pins/go-toolchain)}"; exit;;
  'mod verify') exit;;
esac
printf '%s\n' "${GOCACHE:-shared}|$*" >> "$BUILD_LOG"
out=
while [ $# -gt 0 ]; do if [ "$1" = -o ]; then shift; out=$1; fi; shift; done
case "$out" in */pack)
cat > "$out" <<'PACK'
#!/bin/sh
while [ $# -gt 0 ]; do if [ "$1" = --dir ]; then shift; dir=$1; fi; shift; done
for file in SHA256SUMS; do printf 'same bytes' > "$dir/$file"; done
if [ -n "${DIFFER:-}" ]; then case "$dir" in */second) printf changed > "$dir/SHA256SUMS";; esac; fi
PACK
chmod 700 "$out";;
*) printf fixture > "$out";;
esac
`
	writeFixtureCommand(t, filepath.Join(dir, "bin/go"), source)
	return dir
}
func buildScript(t *testing.T, dir string, values map[string]string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "hack/release-bytes.sh", "reproduce")
	command.Dir = dir
	command.Env = scriptEnv("PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "VERSION=v1.2.3", "GOTMPDIR="+filepath.Join(dir, "scratch"), "BUILD_LOG="+filepath.Join(dir, "build.log"),
		"RELEASE_DIR="+filepath.Join(dir, "dist"), "RELEASE_BASE="+filepath.Join(dir, "base-image"), "RELEASE_CACHE_A="+filepath.Join(dir, "cache-first"), "RELEASE_CACHE_B="+filepath.Join(dir, "cache-second"))
	for key, value := range values {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}
func TestReleaseBuildUsesTreeToolchainPin(t *testing.T) {
	pin := releaseGoVersion(t)
	for _, tc := range []struct {
		name, version string
		refused       bool
	}{
		{"pinned", pin, false},
		{"different", nextGoPatch(t, pin), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := releaseBuildTree(t)
			output, err := buildScript(t, dir, map[string]string{"TOOLCHAIN": tc.version})
			if tc.refused {
				if err == nil || !strings.Contains(string(output), "Go toolchain differs;") || !strings.Contains(string(output), "Found Go: "+tc.version) || !strings.Contains(string(output), "Required Go: "+pin) {
					t.Fatal("different toolchain accepted", err, string(output))
				}
				if _, err := os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
					t.Fatal("compiled with a different toolchain", err)
				}
			} else if err != nil || !strings.Contains(string(output), "Release bytes built.") {
				t.Fatal("tree's pinned toolchain refused", err, string(output))
			}
		})
	}
}
func TestReleaseBuildUsesIndependentCaches(t *testing.T) {
	dir := releaseBuildTree(t)
	output, err := buildScript(t, dir, nil)
	if err != nil {
		t.Fatal(err, string(output))
	}
	body, err := os.ReadFile(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	caches := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		cache, args, _ := strings.Cut(line, "|")
		if strings.Contains(args, "./cmd/oberth ") || strings.HasSuffix(args, "./cmd/oberth") || strings.HasSuffix(args, "./cmd/oberth-source-plugin") {
			caches[cache]++
		}
	}
	if len(caches) != 2 {
		t.Fatal("builds share a cache", caches)
	}
	for cache, count := range caches {
		if count != 6 || !strings.Contains(cache, "/cache-") {
			t.Fatal("pass lacks its own compile cache", caches)
		}
	}
}
func TestReleaseBuildRefusesBeforeCompilation(t *testing.T) {
	for _, tc := range []struct{ name, key, value, reason string }{
		{"dirty", "DIRTY", " M source", "tree is dirty"},
		{"toolchain", "TOOLCHAIN", nextGoPatch(t, releaseGoVersion(t)), "toolchain differs"},
		{"output", "RELEASE_DIR", "base-image", "output exists"},
		{"version", "VERSION", "v01.2.3", "version is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := releaseBuildTree(t)
			output, err := buildScript(t, dir, map[string]string{tc.key: tc.value})
			if err == nil || !strings.Contains(string(output), tc.reason) {
				t.Fatal("missing preflight refusal", err, string(output))
			}
			if _, err := os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
				t.Fatal("compiled before refusing", err)
			}
		})
	}
}
func TestReleaseBuildReportsDifferentBytes(t *testing.T) {
	dir := releaseBuildTree(t)
	output, err := buildScript(t, dir, map[string]string{"DIFFER": "yes"})
	if err == nil || !strings.Contains(string(output), "Release builds differ;") {
		t.Fatal(err, string(output))
	}
}

func TestVerificationIgnoresWorkingDirectoryKey(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "oberth-release")
	command := exec.CommandContext(t.Context(), "go", "build", "-p=1", "-buildvcs=false", "-o", binary, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	for _, name := range []string{"internal/releaseverify", "dist"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// A current-directory pin must never replace the compiled trust root.
	if err := os.WriteFile(filepath.Join(dir, "internal/releaseverify/release-cosign.pub"), []byte("attacker-controlled pin"), 0644); err != nil {
		t.Fatal(err)
	}
	detail := exec.CommandContext(t.Context(), binary, "base", "--base", filepath.Join(dir, "missing-base"))
	if output, err := detail.CombinedOutput(); err == nil || !strings.Contains(string(output), "Detail:") || !strings.Contains(string(output), "missing-base") {
		t.Fatal("native input detail was lost", err, string(output))
	}
	for _, cwd := range []string{dir, filepath.Join(dir, "dist")} {
		command = exec.CommandContext(t.Context(), binary, "key")
		command.Dir = cwd
		output, err := command.CombinedOutput()
		assertCompiledKey(t, output, err)
	}
}

func assertCompiledKey(t *testing.T, output []byte, err error) {
	t.Helper()
	compiled, keyErr := releaseverify.PublicKey()
	if keyErr == nil {
		if err != nil || string(output) != string(compiled) {
			t.Fatal("compiled key changed with working directory")
		}
		return
	}
	if err == nil || !strings.Contains(string(output), releaseverify.Key.Error()) {
		t.Fatal("unprovisioned key refusal missing", err)
	}
}

func TestPackageRefusesInvalidSemVer(t *testing.T) {
	o := setup(t)
	packageFixture(t, o)
	for _, version := range []string{"v1.2.3-01", "v01.2.3", "1.2.3", ""} {
		o.version = version
		if err := packageRelease(t.Context(), o); !errors.Is(err, inputs) {
			t.Fatal("invalid chart version accepted", version, err)
		}
	}
}

func TestMissingBasePinHasItsOwnRemedy(t *testing.T) {
	o := setup(t)
	if err := os.Remove(o.pin); err != nil {
		t.Fatal(err)
	}
	if _, err := checkedBase(o); !errors.Is(err, basePin) {
		t.Fatal("missing pin confused with mismatching base", err)
	}
}
