package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheConstructionHonorsCanceledLifetime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := t.TempDir()
	cache, err := New(ctx, Config{Root: root, Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if cache != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled constructor opened cache", err)
	}
	if _, err := os.Stat(filepath.Join(root, "oberth.gitconfig")); !os.IsNotExist(err) {
		t.Fatal("canceled construction wrote configuration", err)
	}
}
