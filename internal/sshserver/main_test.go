package sshserver

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Real receive-pack fixtures invoke this executable through Git-owned hooks,
	// exactly as the server binary does. Never rerun the test suite as a hook.
	if code, handled := gitcache.ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
