package gitcache_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestCacheRootProcessOwnership(t *testing.T) {
	if root := os.Getenv("OBERTH_TEST_OWNED_ROOT"); root != "" {
		cache, err := gitcache.New(t.Context(), gitcache.Config{Root: root, Scratch: root + "-scratch", Binary: "/usr/bin/git"})
		if err == nil {
			_ = cache.Close()
			t.Fatal("second process opened an owned cache")
		}
		if !strings.Contains(err.Error(), "already owned") {
			t.Fatal(err)
		}
		return
	}
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(f.Store(), "HEAD.lock")
	if err := os.WriteFile(lock, []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), binary, "-test.run=^TestCacheRootProcessOwnership$")
	child.Env = append(os.Environ(), "OBERTH_TEST_OWNED_ROOT="+cfg.Root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("ownership child: %v %s", err, output)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("second process removed live Git lock", err)
	}
	_ = c.Close()
	_ = reviewNew(t, cfg)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("restart did not remove abandoned lock", err)
	}
}
