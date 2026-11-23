//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestAdministratorPromotesTestedCommitFromEmptyDirectory(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	base := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	f.git.Write(t, "change.txt", []byte("tested change\n"))
	commit := f.git.CommitFileAt(t, "branch change", "2000-01-02T00:00:00Z")
	command := newRepoAddCommand(t, f)
	command.app.directory = t.TempDir()
	args := []string{"promote", commit, "--repo", gittest.Repository}
	if code := command.app.run(t.Context(), args); code == 0 {
		t.Fatal("unknown commit promoted from empty directory")
	}
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/topic")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if code := command.app.run(t.Context(), args); code == 0 {
		t.Fatal("untested commit promoted from empty directory")
	}
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != base {
		t.Fatal("refused promotion moved main", head)
	}
	f.sourcePod(t, run)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	f.waitPublication(t, "publication:"+run.ID)
	command.out.Reset()
	command.diagnostic.Reset()
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	if code := command.app.run(ctx, args); code != 0 {
		t.Fatal("tested server commit required a checkout", code, command.diagnostic.String())
	}
	for _, fact := range []string{"tested run: " + run.ID, "main commit: " + commit, "result: passed"} {
		if !strings.Contains(command.out.String(), fact) {
			t.Fatal("promotion omitted verified outcome", command.out.String())
		}
	}
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != commit {
		t.Fatal("server did not publish the tested commit", head)
	}
	if entries, err := os.ReadDir(command.app.directory); err != nil || len(entries) != 0 {
		t.Fatal("promotion needed local files", entries, err)
	}
}
