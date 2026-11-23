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

func TestEmptiedEstablishedForgeRefusesRecentAndExpiredPushes(t *testing.T) {
	f, first := firstEmptyForgePush(t)
	f.finishNative(t, first)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	f.waitPublication(t, "publication:"+first.ID)
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	for _, ref := range []string{"refs/heads/keep", "refs/tags/v1"} {
		f.git.Git(t, f.git.Bare, "update-ref", ref, *first.CommitSHA)
	}
	gittest.ExpireRefresh(t, store)
	if err := f.owners.cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("refresh of established forge", err)
	}
	before := f.git.Git(t, store, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, ref := range []string{"refs/heads/main", "refs/heads/keep", "refs/tags/v1"} {
		f.git.Git(t, f.git.Bare, "update-ref", "-d", ref)
	}
	f.git.Write(t, "later.txt", []byte("later change\n"))
	f.git.CommitFile(t, "change after forge was emptied")
	for _, refresh := range []string{"recent", "expired"} {
		if refresh == "expired" {
			gittest.ExpireRefresh(t, store)
		}
		if output, err := f.pushResult(t, "refs/heads/main"); err == nil || !strings.Contains(output, gitport.ForgeHistoryLostReason) {
			t.Fatal("emptied established forge accepted main with "+refresh+" refresh", err, output)
		}
		if after := f.git.Git(t, store, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
			t.Fatal("refused push changed cached heads or tags", refresh, before, after)
		}
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("emptied forge was repopulated", refs)
	}
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	if err != nil || len(runs) != 1 {
		t.Fatal("refused push started CI", len(runs), err)
	}
}
