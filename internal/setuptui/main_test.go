package setuptui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// TestMain isolates the wizard's tests from the machine running them: no
// helm/kubectl check (CI has no kubectl), no real kubeconfig, no kubectl or
// installer run against whatever cluster this host has, and a throwaway
// home and config directory so setup.json and reports never land in the
// developer's own ~/.config.
func TestMain(m *testing.M) {
	checkHostTools = func(string, func(string) (string, error)) error { return nil }
	runKubectl = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("test ran an unstubbed kubectl")
	}
	executeInstaller = func(context.Context, installer.Config, installer.InstallDeps) error {
		return errors.New("test ran the real installer")
	}
	// A saved install is assumed present unless a test says otherwise.
	releasePresent = func(context.Context, string, string, string) (bool, bool) { return true, true }
	dir, err := os.MkdirTemp("", "setuptui-test-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"HOME":            dir,
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"KUBECONFIG":      filepath.Join(dir, "no-kubeconfig"),
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
