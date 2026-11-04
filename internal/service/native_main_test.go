package service

import (
	"context"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestMain(m *testing.M) {
	if code, handled := gitcache.ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
