package gitcache

import (
	"context"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if code, handled := ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}
	if code, handled := nativeFixtureInvocation(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
