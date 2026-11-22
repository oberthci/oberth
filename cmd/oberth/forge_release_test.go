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

// A release never tags from a cache the server served instead of the forge:
// neither after a forge that did not answer nor while the forge copy is
// still updating, and each says which.
func TestReleaseHeadRefusesCachedForgeNoticeBeforeTagging(t *testing.T) {
	for _, tc := range []struct{ notice, reason, remedy string }{
		{gitport.ForgeCacheNotice, gitport.ForgeReason, gitport.ForgeRemedy},
		{gitport.ForgeUpdatingNotice, gitport.FetchUnfinishedReason, gitport.FetchUnfinishedRemedy},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newReleaseFixture(t)
			binary, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			script := "#!/bin/sh\nif [ \"$1\" = fetch ]; then\n echo '" + tc.notice + "1m0s.' >&2\n exit 0\nfi\nexec '" + binary + "' \"$@\"\n"
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
			if !strings.Contains(message, tc.reason) || !strings.Contains(message, tc.remedy) {
				t.Fatal(f.diagnostic.String())
			}
		})
	}
}
