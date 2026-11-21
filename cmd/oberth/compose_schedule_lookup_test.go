//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/runtime"
	ktest "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
)

// However often schedules are listed, a waiting entry costs the forge one
// question per repository per freshness interval: the cache's markers do not
// move between the first list and the tenth, and move again after the interval.
func TestScheduleListAsksTheForgeOncePerInterval(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "a schedule on a branch that is nowhere")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.scheduleAccount(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	markers := func() string {
		var text string
		for _, name := range []string{"oberth-refreshed", "oberth-looked"} {
			data, err := os.ReadFile(filepath.Join(f.cacheStore(), name))
			if err != nil {
				t.Fatal("a list asked the forge without a record", name, err)
			}
			text += string(data) + "\n"
		}
		return text
	}
	for _, round := range []string{"first interval", "after the interval"} {
		gittest.ExpireRefresh(t, f.cacheStore())
		if reason := f.scheduleReason(t, "weekly"); reason != "schedule branch not found" {
			t.Fatal(round, "entry changed", reason)
		}
		first := markers()
		for range 9 {
			f.scheduleReason(t, "weekly")
		}
		if markers() != first {
			t.Fatal(round, "repeated lists asked the forge again")
		}
	}
}

// A forge that refuses Git or never answers changes nothing: schedule list
// prints the entry as it was well inside the read's budget and exits 0, and
// schedule trigger still refuses with the entry's own reason and remedy.
func TestScheduleLookChangesNothingWhenTheForgeDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "a schedule on a branch that is nowhere")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.scheduleAccount(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	refusing, silent := *f.forge.config.Load(), *f.forge.config.Load()
	refusing.NoClientAuth, silent.NoClientAuth = false, false
	refusing.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, errors.New("forge is not serving Git")
	}
	silent.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		<-t.Context().Done()
		return nil, t.Context().Err()
	}
	for name, forge := range map[string]*ssh.ServerConfig{"refusing": &refusing, "silent": &silent} {
		f.forge.config.Store(forge)
		gittest.ExpireRefresh(t, f.cacheStore())
		started := time.Now() //nolint:forbidigo // The read budget is wall time on a real socket.
		if reason := f.scheduleReason(t, "weekly"); reason != "schedule branch not found" {
			t.Fatal(name, "forge changed the entry", reason)
		}
		if took := time.Since(started); took > 8*time.Second { //nolint:forbidigo // As above.
			t.Fatal(name, "list waited for the forge", took)
		}
		app, _, diagnostic := f.revokeApplication(t)
		args := []string{"schedule", "trigger", "--repo", gittest.Repository, "--entry", "weekly", "--reason", "look"}
		if code := app.run(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "Schedule not admitted: schedule branch not found.") || !strings.Contains(diagnostic.String(), "git push oberth <branch>") {
			t.Fatal(name, "trigger lost the entry's reason", code, diagnostic.String())
		}
	}
}

// With several repositories waiting for a branch and a forge that never
// answers, one look deadline covers the whole list: it answers inside the
// read's budget, exits 0 and leaves every entry as it was.
func TestScheduleListOfManyWaitingRepositoriesAnswersWhenTheForgeDoesNot(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "a schedule on a branch that is nowhere")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	repositories := []string{gittest.Repository, "origin/team/gadget", "origin/team/gizmo"}
	for _, repo := range repositories[1:] {
		f.git.Git(t, f.git.Root, "clone", "-q", "--bare", f.git.Bare, filepath.Join(f.git.Root, strings.TrimPrefix(repo, "origin/team/")+".git"))
	}
	for _, repo := range repositories {
		f.pipelineAccount(t, repo)
	}
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	for _, repo := range repositories[1:] {
		if _, err := f.owners.c.store.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, repo); err != nil {
			t.Fatal(err)
		}
		if err := f.owners.c.schedules.SyncRepository(t.Context(), repo); err != nil {
			t.Fatal("reconcile", repo, err)
		}
	}
	if waiting := f.waitingRepositories(t); len(waiting) != 3 {
		t.Fatal("fixture repositories are not all waiting", waiting)
	}
	silent := *f.forge.config.Load()
	silent.NoClientAuth = false
	silent.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		<-t.Context().Done()
		return nil, t.Context().Err()
	}
	f.forge.config.Store(&silent)
	for _, repo := range repositories {
		gittest.ExpireRefresh(t, filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", repo+".git"))
	}
	started := time.Now() //nolint:forbidigo // The read budget is wall time on a real socket.
	waiting := f.waitingRepositories(t)
	if took := time.Since(started); took > 8*time.Second || len(waiting) != 3 { //nolint:forbidigo // As above.
		t.Fatal("the list did not answer inside its budget with every entry as it was", took, waiting)
	}
}

// waitingRepositories lists every schedule and returns the repositories whose
// entry still waits for its branch.
func (f *composedFixture) waitingRepositories(t *testing.T) []string {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"schedule", "list", "-o", "json"}); code != 0 {
		t.Fatal("schedule list failed", code, diagnostic.String())
	}
	var page apicontract.SchedulePage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var waiting []string
	for _, s := range page.Schedules {
		if s.DisplayReason == "schedule branch not found" {
			waiting = append(waiting, s.Repo)
		}
	}
	return waiting
}

// The look hands what moved to the ref-notice queue and the list returns: a
// reconciliation held up past the look's deadline still admits the entry, and
// the list that looked does not wait for it.
func TestScheduleListHandsTheMoveToTheNoticeQueue(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: weekly, cron: '0 4 * * 1', branch: weekly}]\n"))
	f.git.CommitFile(t, "a schedule on a branch that is not there yet")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.scheduleAccount(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	if reason := f.scheduleReason(t, "weekly"); reason != "schedule branch not found" {
		t.Fatal("fixture entry is not waiting", reason)
	}
	release := make(chan struct{})
	f.argo.PrependReactor("create", "cronworkflows", func(ktest.Action) (bool, runtime.Object, error) {
		select {
		case <-release:
		case <-time.After(9 * time.Second):
		}
		return false, nil, nil
	})
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/weekly")
	gittest.ExpireRefresh(t, f.cacheStore())
	started := time.Now() //nolint:forbidigo // The list's wait is wall time.
	reason := f.scheduleReason(t, "weekly")
	took := time.Since(started) //nolint:forbidigo // As above.
	close(release)
	if took > 3*time.Second || reason != "schedule branch not found" {
		t.Fatal("the list waited for the reconciliation or printed more than it found", took, reason)
	}
	if !f.awaitScheduleAdmitted(t, "weekly") || f.scheduleReason(t, "weekly") != "" {
		t.Fatal("the queued reconciliation did not admit the entry", f.trace.String())
	}
}

// cacheStore is the server's Git cache directory for the fixture repository.
func (f *composedFixture) cacheStore() string {
	return filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
}

// Looking at a schedule is a read through Oberth: with no Git session at all,
// `schedule list` admits an entry whose branch reached only the forge, and
// `schedule trigger` of such an entry runs it, each after the one bounded
// refresh the reconciliation already does.
func TestScheduleListAndTriggerLookAtTheForgeForAMissingBranch(t *testing.T) {
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
	for _, entry := range []string{"nightly", "weekly"} {
		if reason := f.scheduleReason(t, entry); reason != "schedule branch not found" || f.scheduleAdmitted(t, entry) {
			t.Fatal("absent branch admitted", entry, reason)
		}
	}
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/nightly")
	gittest.ExpireRefresh(t, f.cacheStore()) // the earlier lists looked; the interval passes
	f.scheduleReason(t, "nightly")           // the look; its reconciliation runs after the list
	if !f.awaitScheduleAdmitted(t, "nightly") || f.scheduleReason(t, "nightly") != "" {
		t.Fatal("schedule list did not look at the forge", f.trace.String())
	}
	if f.scheduleAdmitted(t, "weekly") {
		t.Fatal("a branch on no remote was admitted")
	}
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/weekly")
	gittest.ExpireRefresh(t, f.cacheStore())
	app, out, diagnostic := f.revokeApplication(t)
	args := []string{"schedule", "trigger", "--repo", gittest.Repository, "--entry", "weekly", "--reason", "first occurrence"}
	if code := app.run(t.Context(), args); code != 0 || !f.scheduleAdmitted(t, "weekly") {
		t.Fatal("schedule trigger did not look at the forge", code, out.String(), diagnostic.String())
	}
}
