//go:build linux

// This test exercises the Linux server and Git receive path.

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestEmptyForgeFailedFirstRunCanBeFixed(t *testing.T) {
	f, first := firstEmptyForgePush(t)
	f.endNative(t, first, 1)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunFailed })
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("failed first run published", refs)
	}
	if head, err := f.owners.cache.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil || head.CommitSHA != *first.CommitSHA {
		t.Fatal("failed first run lost the accepted default head", head, err)
	}
	f.git.Write(t, ".oberth/build.yaml", []byte(composedWorkflow+"# fixed pipeline\n"))
	commit := f.git.CommitFile(t, "fix first pipeline")
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	gittest.ExpireRefresh(t, store)
	if output, err := f.pushResult(t, "refs/heads/main"); err != nil {
		t.Fatal("failed first run blocked its fix", err, output)
	}
	run := f.waitMatchingRun(t, func(r stateport.Run) bool {
		return r.CommitSHA != nil && *r.CommitSHA == commit && r.Origin.WorkflowUID != ""
	})
	if run.ID == first.ID || run.Ref != "refs/heads/main" {
		t.Fatal("fix did not create an ordinary main run", run.RunSummary)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("fix published before passing CI", refs)
	}
	f.sourcePod(t, run)
	f.finishNative(t, run)
	f.waitMatchingRun(t, func(r stateport.Run) bool { return r.ID == run.ID && r.Status == stepmodel.RunPassed })
	f.waitPublication(t, "publication:"+run.ID)
	f.checkPublishedForgeHistory(t, commit)
}

func (f *composedFixture) checkPublishedForgeHistory(t *testing.T, commit string) {
	t.Helper()
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != commit {
		t.Fatal("forge lacks the passing fix", head, commit)
	}
	f.git.Git(t, f.git.Bare, "update-ref", "-d", "refs/heads/main")
	f.git.Write(t, "later.txt", []byte("later change\n"))
	f.git.CommitFile(t, "change after forge was emptied")
	gittest.ExpireRefresh(t, store)
	if output, err := f.pushResult(t, "refs/heads/main"); err == nil || !strings.Contains(output, gitport.EmptyForgeReason) {
		t.Fatal("published history was treated as bootstrap", err, output)
	}
	if head := f.git.Git(t, store, "rev-parse", "refs/heads/main"); head != commit {
		t.Fatal("empty forge changed established main", head, commit)
	}
}
