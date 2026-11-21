package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
)

func TestReleaseHeadRefusesCachedForgeNoticeBeforeTagging(t *testing.T) {
	f := newReleaseFixture(t)
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = fetch ]; then\n echo '" + gitport.ForgeCacheNotice + "1m0s.' >&2\n exit 0\nfi\nexec '" + binary + "' \"$@\"\n"
	if err = os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	base := f.app.git
	f.app.git = func(ctx context.Context, args []string) (string, error) {
		if args[0] == "fetch" {
			return runGit(ctx, f.app.directory, args...)
		}
		return base(ctx, args)
	}
	if code := f.run(t); code != 1 {
		t.Fatal("release accepted a stale cache", code)
	}
	assertNoReleaseEffects(t, f)
	message := strings.Join(strings.Fields(f.diagnostic.String()), " ")
	if !strings.Contains(message, gitport.ForgeReason) || !strings.Contains(message, gitport.ForgeRemedy) {
		t.Fatal(f.diagnostic.String())
	}
}
