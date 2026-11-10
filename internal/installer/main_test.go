package installer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// TestMain gives the whole test binary a throwaway home directory. Onboarding
// and client-access paths write ~/.ssh/config and ~/.config/oberth, and many
// tests run in parallel, where t.Setenv is unavailable. Without this, a test
// that reaches one of those writers edits the operator's real files (seen on
// example-node: ~/.config/oberth/ca.crt held a stubbed command's output).
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
		"KUBECONFIG":      "",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	// No test reaches the real release origins: a test that needs a signed
	// release serves one (withSignedRelease).
	defaultReleaseGetter = func(_ context.Context, url string, _ int64) ([]byte, error) {
		return nil, fmt.Errorf("fetch %s: HTTP 404 (tests never reach the network)", url)
	}
	// client-go caches this path before TestMain changes the environment.
	clientcmd.RecommendedHomeFile = filepath.Join(home, ".kube", "config")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
