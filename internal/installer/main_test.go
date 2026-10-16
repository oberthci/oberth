package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the whole test binary a throwaway home directory. Onboarding
// and client-access paths write ~/.ssh/config and ~/.config/oberth, and many
// tests run in parallel, where t.Setenv is unavailable. Without this, a test
// that reaches one of those writers edits the operator's real files (seen on
// tuxbox: ~/.config/oberth/ca.crt held a stubbed command's output).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "oberth-installer-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for key, value := range map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
