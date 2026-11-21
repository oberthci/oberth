package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func releaseGoVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../.oberth/pins/go-toolchain")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func nextGoPatch(t *testing.T, version string) string {
	t.Helper()
	fields := strings.Split(strings.TrimSpace(version), ".")
	if len(fields) != 3 {
		t.Fatal("toolchain pin has no exact patch", version)
	}
	patch, err := strconv.Atoi(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	fields[2] = strconv.Itoa(patch + 1)
	return strings.Join(fields, ".")
}

func TestReleaseBuildDoesNotSelectAmbientToolchain(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(t.Context(), goTool, "env", "GOVERSION")
	probe.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	version, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	other := nextGoPatch(t, string(version))
	dir := releaseBuildTree(t)
	if err = os.WriteFile(filepath.Join(dir, ".oberth/pins/go-toolchain"), []byte(other+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeFixtureCommand(t, filepath.Join(dir, "bin/go"), `if [ "$*" != 'env GOVERSION' ]; then
printf unexpected > "$BUILD_LOG"
exit 99
fi
exec "$REAL_GO" "$@"
`)
	output, err := buildScript(t, dir, map[string]string{
		"REAL_GO": goTool, "GOTOOLCHAIN": other + "+auto", "GOPROXY": "off", "GOSUMDB": "off",
	})
	if err == nil || !strings.Contains(string(output), "Go toolchain differs;") || strings.Contains(string(output), "downloading") {
		t.Fatal("release attempted ambient compiler selection", err, string(output))
	}
	if _, err = os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
		t.Fatal("release reached compilation with the wrong local compiler", err)
	}
}

func TestReleaseBuildMissingToolchainPinHasOwnRefusal(t *testing.T) {
	dir := releaseBuildTree(t)
	if err := os.Remove(filepath.Join(dir, ".oberth/pins/go-toolchain")); err != nil {
		t.Fatal(err)
	}
	output, err := buildScript(t, dir, nil)
	if err == nil || !strings.Contains(string(output), "Go toolchain differs;") || strings.Contains(string(output), "cat:") {
		t.Fatal("missing pin did not produce the script's refusal", err, string(output))
	}
	if _, err = os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
		t.Fatal("missing pin reached compilation", err)
	}
}
