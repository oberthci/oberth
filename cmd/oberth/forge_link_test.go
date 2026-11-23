//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestForgeOutageBranchPushStartsExactRun(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	// Make the successful clone older than the bound without waiting in real time.
	marker := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git", "oberth-refreshed")
	if err := os.WriteFile(marker, []byte(f.clock.Now().Add(-time.Minute).Format(time.RFC3339Nano)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.git.Bare, f.git.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	out, err := f.pushResult(t, "topic", "-o", "oberth.run=1")
	if err != nil {
		t.Fatal("forge outage stopped CI", err, out)
	}
	// A topic receive reads no forge state, so it cannot report a live outage.
	if strings.Contains(out, gitport.ForgeCacheNotice) || strings.Contains(out, "fatal:") || strings.Contains(out, "server's own forge link") {
		t.Fatal("wrong outage feedback", out)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.CommitSHA == nil || *run.CommitSHA != commit {
		t.Fatal("wrong admitted commit", run.RunSummary)
	}
	f.deliverSource(t, run)
}
