package gitcache_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestForgeOutageServesCacheAndRefusesPublication(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	var logged []string
	cfg.Log = func(reason, detail string) { logged = append(logged, reason+": "+detail) }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	var out, notice bytes.Buffer
	err := c.Upload(t.Context(), gittest.Repository, gitcache.ServiceIO{Input: io.NopCloser(strings.NewReader("0000")), Output: &out, Diagnostics: &notice})
	if err != nil || !strings.Contains(out.String(), f.Commit+" refs/heads/main") {
		t.Fatal("offline read failed", err, out.String())
	}
	if notice.String() != gitport.ForgeCacheNotice+"1m0s.\n" {
		t.Fatal("missing age or duplicate notice", notice.String())
	}
	for _, operation := range []func() error{
		func() error { _, _, err := c.DefaultFile(t.Context(), gittest.Repository, "file.txt"); return err },
		func() error {
			_, err := c.Publish(t.Context(), publicationFor(f, "refs/heads/main", f.Commit, f.Commit))
			return err
		},
	} {
		err = operation()
		var unavailable *gitport.UnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason() != gitport.ForgeReason || strings.Contains(err.Error(), f.Root) || strings.Contains(err.Error(), "fatal:") {
			t.Fatal("wrong forge refusal", err)
		}
	}
	if !strings.Contains(strings.Join(logged, "\n"), gitport.ForgeOperatorRemedy) {
		t.Fatal("operator remedy missing", logged)
	}
}
