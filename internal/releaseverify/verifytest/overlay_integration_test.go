package verifytest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestCommandLineOverlaySurvivesFixtureRebuild(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"equals", "separate-relative", "overrides-environment", "clears-environment", "environment", "already-pinned"} {
		t.Run(mode, func(t *testing.T) { checkConsumerOverlay(t, root, mode) })
	}
}

func checkConsumerOverlay(t *testing.T, root, mode string) {
	t.Helper()
	cli := consumerOverlay(t, root, "command-line", mode == "separate-relative")
	environment := consumerOverlay(t, root, "environment", false)
	args := []string{"test", "-p=1", "-buildvcs=false", "-count=1", "-timeout=2m", "-run=^TestForwardedOverlay$"}
	flags, want := "", "command-line"
	key, _ := releaseverify.PublicKey()
	if mode == "already-pinned" {
		key = pinConsumerOverlay(t, root, cli)
	}
	switch mode {
	case "equals", "already-pinned":
		args = append(args, "-overlay="+cli)
	case "separate-relative":
		args = append(args, "-overlay", cli)
	case "overrides-environment":
		flags = "'-overlay=" + environment + "'"
		args = append(args, "-overlay="+cli)
	case "clears-environment":
		flags, want = "'-overlay="+environment+"'", "original"
		args = append(args, "-overlay=")
	case "environment":
		flags, want = "'-overlay="+environment+"'", "environment"
	}
	args = append(args, "./internal/releaseverify/verifytest/testdata/overlay")
	command := exec.CommandContext(t.Context(), "go", args...)
	command.Dir = root
	digest := sha256.Sum256(key)
	command.Env = append(os.Environ(), "OBERTH_OVERLAY_COMPILED_PROBE=", "GOFLAGS="+flags, "OBERTH_OVERLAY_EXPECT="+want, "OBERTH_OVERLAY_PRODUCT_PIN_SHA256="+hex.EncodeToString(digest[:]))
	if mode == "equals" {
		directory := filepath.Join(t.TempDir(), "build space")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		command.Env = append(command.Env, "GOTMPDIR="+directory)
	}
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("consumer overlay %s: %v\n%s", mode, err, raw)
	}
}

func consumerOverlay(t *testing.T, root, value string, relative bool) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "overlay space")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "internal/releaseverify/verifytest/testdata/overlay/overlay_test.go")
	target := filepath.Join(directory, value+".go")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`const Value = "original"`), []byte(`const Value = "`+value+`"`), 1)
	if err := os.WriteFile(target, body, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, value+".json")
	if relative {
		var err error
		source, err = filepath.Rel(root, source)
		if err != nil {
			t.Fatal(err)
		}
		target, err = filepath.Rel(root, target)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{"Replace": map[string]string{source: target}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if relative {
		path, err = filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestCompiledOverlayRefusesBlindFixtureRebuild(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	overlay := consumerOverlay(t, root, "command-line", false)
	binary := filepath.Join(t.TempDir(), "consumer.test")
	command := exec.CommandContext(t.Context(), "go", "test", "-c", "-p=1", "-buildvcs=false", "-timeout=2m", "-overlay="+overlay, "-o", binary, "./internal/releaseverify/verifytest/testdata/overlay")
	command.Dir = root
	command.Env = append(os.Environ(), "OBERTH_OVERLAY_COMPILED_PROBE=", "GOFLAGS=")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile consumer: %v\n%s", err, raw)
	}
	command = exec.CommandContext(t.Context(), binary, "-test.timeout=2m", "-test.run=^TestForwardedOverlay$")
	command.Dir = filepath.Join(root, "internal/releaseverify/verifytest/testdata/overlay")
	command.Env = append(os.Environ(), "OBERTH_OVERLAY_COMPILED_PROBE=")
	raw, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "fixture rebuild requires direct go test") {
		t.Fatalf("blind rebuild did not refuse lost build metadata: %v\n%s", err, raw)
	}
}

func pinConsumerOverlay(t *testing.T, root, path string) []byte {
	t.Helper()
	key, err := fixturePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fixture.pem")
	if err := os.WriteFile(file, key, 0600); err != nil {
		t.Fatal(err)
	}
	replacements, err := overlayFiles(root, path)
	if err != nil {
		t.Fatal(err)
	}
	replacements[filepath.Join(root, "internal/releaseverify/release-cosign.pub")] = file
	if err := writeOverlay(path, replacements); err != nil {
		t.Fatal(err)
	}
	return key
}
