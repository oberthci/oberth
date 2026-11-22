//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestEmptyForgeRegistrationAndPushExplainFirstCommit(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.git.Git(t, f.git.Bare, "update-ref", "-d", "refs/heads/main")
	err = (serverAdministration{f.owners}).ProbeRepository(t.Context(), repo)
	problem := publicerr.Classify(err)
	if problem == nil || problem.Reason() != gitport.EmptyForgeReason || problem.Remedy() != gitport.EmptyForgeRemedy {
		t.Fatal("registration probe hid empty forge", err)
	}
	f.checkEmptyUpstream(t)
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	for range 2 {
		gittest.ExpireRefresh(t, store)
		out, err := f.pushResult(t, "topic")
		if err == nil || !strings.Contains(out, gitport.EmptyForgeReason) || !strings.Contains(out, gitport.EmptyForgeRemedy) {
			t.Fatal("push hid empty forge", err, out)
		}
		if _, err = os.Stat(filepath.Join(store, "HEAD")); err == nil {
			if err = f.owners.cache.Remove(t.Context(), gittest.Repository); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// repo add checks the repository through upstream add before binding it; the
// caller gets the cause and remedy, the server log the full cause.
func (f *composedFixture) checkEmptyUpstream(t *testing.T) {
	t.Helper()
	out, err := (serverAdministration{f.owners}).ProbeUpstream(t.Context(), "origin", "team/widget")
	problem := publicerr.Classify(err)
	if !out.Pending || out.DisplayReason != gitport.EmptyForgeReason || problem == nil || problem.Remedy() != gitport.EmptyForgeRemedy {
		t.Fatal("upstream check hid empty forge", out, err)
	}
	if log := f.trace.String(); !strings.Contains(log, `msg="repository check failed" repository=origin/team/widget reason="The forge repository has no commit."`) {
		t.Fatal("server log lacks the check's cause", log)
	}
}
