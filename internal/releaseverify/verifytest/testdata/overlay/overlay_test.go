package overlay

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/releaseverify/verifytest"
)

const Value = "original"

func TestMain(m *testing.M) {
	if os.Getenv("OBERTH_OVERLAY_COMPILED_PROBE") == "1" {
		key, _ := releaseverify.PublicKey()
		fmt.Printf("%s\n%x", Value, sha256.Sum256(key))
		os.Exit(0)
	}
	os.Exit(verifytest.WithKey(m))
}

func TestForwardedOverlay(t *testing.T) {
	want := os.Getenv("OBERTH_OVERLAY_EXPECT")
	if want == "" || Value != want {
		t.Fatalf("rebuilt test binary: got %q, want %q", Value, want)
	}
	binary := filepath.Join(t.TempDir(), "nested.test")
	command := exec.CommandContext(t.Context(), "go", "test", "-c", "-p=1", "-buildvcs=false", "-timeout=2m", "-o", binary, ".")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nested compile: %v\n%s", err, raw)
	}
	command = exec.CommandContext(t.Context(), binary)
	command.Env = append(os.Environ(), "OBERTH_OVERLAY_COMPILED_PROBE=1")
	raw, err := command.CombinedOutput()
	if err != nil || string(raw) != want+"\n"+os.Getenv("OBERTH_OVERLAY_PRODUCT_PIN_SHA256") {
		t.Fatalf("nested build lost the caller overlay or changed the product pin: %v", err)
	}
}
