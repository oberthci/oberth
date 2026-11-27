package gitcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestReleaseAncestryRequiresFreshForgeHead(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		savedAnswer      bool
	}{
		{name: "silent forge with cached refs"},
		{name: "silent forge with saved answer", savedAnswer: true},
		{name: "unreachable forge", diagnostic: gitssh.ReasonUnanswered, savedAnswer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkReleaseAncestry(t, tc.diagnostic, tc.savedAnswer)
		})
	}
}

func checkReleaseAncestry(t *testing.T, diagnostic string, savedAnswer bool) {
	t.Helper()
	f := gittest.New(t)
	f.Write(t, "removed", []byte("release candidate"))
	removed := f.CommitFile(t, "release candidate")
	f.Git(t, f.Work, "push", "-q", f.Bare, "main")
	cfg := reviewConfig(t, f)
	forge := holdForges(t, &cfg, f.Binary, map[string]string{"origin": f.Bare})
	c := reviewNew(t, cfg)
	t.Cleanup(forge.release)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	at := sourcecontract.Revision{Repository: gittest.Repository, Commit: removed}
	head, reachable, err := c.DefaultAncestor(t.Context(), at)
	if err != nil || !reachable || head.CommitSHA != removed {
		t.Fatal("fresh reachable release was refused", head, reachable, err)
	}
	if savedAnswer {
		assertPin(t, c, at, pinWant{1, true})
	}
	f.Git(t, f.Work, "reset", "-q", "--hard", f.Commit)
	f.Write(t, "rewritten", []byte("different default history"))
	f.CommitFile(t, "rewrite default history")
	f.Git(t, f.Work, "push", "-q", "--force", f.Bare, "main")
	forge.arm(t)
	if diagnostic != "" {
		forge.failWith(t, diagnostic)
	}
	gitcache.SetHeadWait(c, 300*time.Millisecond)
	head, reachable, err = c.DefaultAncestor(t.Context(), at)
	if reachable || head.CommitSHA != "" {
		t.Fatal("release trusted stale default history", head, reachable, err)
	}
	checkReleaseHeadRefusal(t, err)
	if got := forge.asked(t, "origin"); got != 1 {
		t.Fatal("release head read was retried", got)
	}
	assertPin(t, c, at, pinWant{1, true})
}

func checkReleaseHeadRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Code() != publicerr.Unavailable || refusal.Reason() != "release default head is unavailable" {
		t.Fatal("release did not report unavailable default head", err)
	}
	if refusal.Remedy() != "Try again once the forge answers." || refusal.Detail != "Forge of "+gittest.Repository+"." {
		t.Fatal("release did not explain how to recover", refusal)
	}
	if publicerr.CheckMessage(refusal.Reason(), 40) != nil || publicerr.CheckMessage(refusal.Remedy(), 120) != nil {
		t.Fatal("release refusal exceeds message bounds", refusal)
	}
}
