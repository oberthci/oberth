package installer_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/releaseverify/verifytest"
)

// Consumer tests build the canonical verifier with a fixture embed overlay.
func TestMain(m *testing.M) {
	expected := releaseverify.Key
	if _, err := releaseverify.PublicKey(); err == nil {
		expected = releaseverify.Missing
	}
	if _, err := installer.LoadRelease(context.Background(), fstest.MapFS{}, "v0.1.0"); !errors.Is(err, expected) || !strings.Contains(err.Error(), expected.Remedy()) {
		fmt.Fprintln(os.Stderr, "production verifier did not refuse incomplete input", err)
		os.Exit(1)
	}
	os.Exit(verifytest.WithKey(m))
}
