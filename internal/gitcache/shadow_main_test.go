package gitcache

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// Exercise newShadowState's actual default executable/entrypoint in the test
// binary, without substituting a custom ShadowCommand for that wiring test.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "internal-gitcache-shadow" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		code := RunShadowHelper(ctx, os.Args[2:], os.Stdout)
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}
