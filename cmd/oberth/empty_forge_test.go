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
