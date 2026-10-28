package app

import (
	"context"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

// Git receive fixtures run the actual native hook via this test executable.
func TestMain(m *testing.M) {
	if code, handled := gitcache.ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
