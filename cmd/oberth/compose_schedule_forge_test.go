//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A repository that was never pushed through Oberth: its schedule names one
// branch that reaches the forge later and one that is nowhere. The forge's
// branch is admitted as soon as a Git session sees it; the other refuses with
// a remedy that, followed literally, admits it; a trigger before that names
// the entry's reason and what to run.
func TestComposedScheduleBranchOnTheForgeIsAdmitted(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: nightly, cron: '0 3 * * *', branch: nightly}, {name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "schedules on their own branches")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.scheduleAccount(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindOperatorPush(t)
	for _, entry := range []string{"nightly", "weekly"} {
		if reason := f.scheduleReason(t, entry); reason != "schedule branch not found" || f.scheduleAdmitted(t, entry) {
			t.Fatal("absent branch admitted", entry, reason)
		}
	}
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/nightly")
	gittest.ExpireRefresh(t, filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git"))
	// The printed remedy: the cache already has the branch, so nothing is sent.
	f.git.Git(t, f.git.Work, "push", "oberth", "HEAD:refs/heads/nightly")
	if !f.awaitScheduleAdmitted(t, "nightly") {
		t.Fatal("a schedule branch on the forge was not admitted", f.scheduleReason(t, "nightly"), f.trace.String())
	}
	app, _, diagnostic := f.revokeApplication(t)
	args := []string{"schedule", "trigger", "--repo", gittest.Repository, "--entry", "weekly", "--reason", "first occurrence"}
	if code := app.run(t.Context(), args); code != 1 ||
		!strings.Contains(diagnostic.String(), "Schedule not admitted: schedule branch not found.") ||
		!strings.Contains(diagnostic.String(), "git push oberth <branch>") {
		t.Fatal("trigger refusal does not name the entry's reason and command", code, diagnostic.String())
	}
	f.git.Git(t, f.git.Work, "branch", "weekly")
	f.git.Git(t, f.git.Work, "push", "oberth", "weekly")
	if !f.awaitScheduleAdmitted(t, "weekly") {
		t.Fatal("the printed remedy did not admit the schedule", f.scheduleReason(t, "weekly"))
	}
}

// A forge that does not answer is never a reason for the server not to start:
// restarted shortly after a refresh, with the forge refusing Git and an entry on
// a branch the cache lacks, the server is ready and the entry stays
// "schedule branch not found".
func TestComposedRestartWithTheForgeDownAndAnAbsentScheduleBranch(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "a schedule on a branch that does not exist yet")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.scheduleAccount(t)
	cancel, done := f.run(t)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	down := *f.forge.config.Load()
	down.NoClientAuth = false
	down.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, errors.New("forge is not serving Git")
	}
	f.forge.config.Store(&down)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("first server stop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("first server did not stop")
	}
	f.restartChannels(t)
	stop, restarted := f.run(t)
	defer stop()
	f.checkStarting(t, restarted)
	f.waitStarted(t, restarted)
	if reason := f.scheduleReason(t, "weekly"); reason != "schedule branch not found" {
		t.Fatal("the outage replaced the entry's reason", reason)
	}
}

func (f *composedFixture) scheduleReason(t *testing.T, entry string) string {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"schedule", "list", "--repo", gittest.Repository, "-o", "json"}); code != 0 {
		t.Fatal("schedule list failed", code, diagnostic.String())
	}
	var page apicontract.SchedulePage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Schedules {
		if s.Entry == entry {
			return s.DisplayReason
		}
	}
	return ""
}

// awaitScheduleAdmitted waits, bounded, for the reconciliation a ref notice
// hands to the server: it runs after the Git session that saw the move returned.
func (f *composedFixture) awaitScheduleAdmitted(t *testing.T, entry string) bool {
	t.Helper()
	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !f.scheduleAdmitted(t, entry) {
		select {
		case <-tick.C:
		case <-deadline:
			return false
		}
	}
	return true
}

func (f *composedFixture) scheduleAdmitted(t *testing.T, entry string) bool {
	t.Helper()
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	name, err := nativebinding.CronName(repo.ID, entry)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.argo.ArgoprojV1alpha1().CronWorkflows(f.config.PipelineNamespace).Get(t.Context(), name, meta.GetOptions{})
	return err == nil
}
