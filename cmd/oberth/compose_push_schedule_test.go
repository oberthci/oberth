//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComposedPostStartPushListsPausesAndResumesSchedule(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.assertScheduleCLI(t, nil)
	f.bindOperatorPush(t)
	f.scheduleAccount(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1")))
	// This source-free schedule is absent during recovery; only the ref notification can publish it.
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]\n"))
	commit := f.git.CommitFile(t, "add nightly after readiness")
	f.git.Git(t, f.git.Work, "push", "oberth", "HEAD:refs/heads/main")
	if !f.awaitScheduleAdmitted(t, "nightly") {
		t.Fatal("the pushed schedule was not reconciled", f.trace.String())
	}
	f.assertScheduleCLI(t, new(false))
	f.scheduledParent(t, commit)
	for _, action := range []struct {
		name   string
		paused bool
	}{{"pause", true}, {"resume", false}} {
		app, _, diagnostic := f.revokeApplication(t)
		args := []string{"schedule", action.name, "--repo", gittest.Repository, "--entry", "nightly", "--reason", "reviewed schedule change"}
		if code := app.run(t.Context(), args); code != 0 {
			t.Fatal("schedule action failed", action.name, code, diagnostic.String())
		}
		f.assertScheduleCLI(t, &action.paused)
		parent := f.scheduledParent(t, commit)
		if parent.Spec.Suspend != action.paused {
			t.Fatal("native schedule pause state differs", parent.Spec.Suspend, action.paused)
		}
	}
}

func (f *composedFixture) bindOperatorPush(t *testing.T) {
	t.Helper()
	private, public := composeSSHKey(t, t.TempDir(), "operator-push.key")
	key := stateport.Key{Identity: "operator", Fingerprint: ssh.FingerprintSHA256(public), Kind: "operator", Admin: true}
	if err := f.owners.c.store.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key); err != nil {
		t.Fatal(err)
	}
	// The SQL authority and SSH gate authenticate this key on the actual receive path.
	raw, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.WritePrivate(f.identity, raw); err != nil {
		t.Fatal(err)
	}
	f.bindClient(t)
}

func (f *composedFixture) assertScheduleCLI(t *testing.T, paused *bool) {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"schedule", "list", "--repo", gittest.Repository, "-o", "json"}); code != 0 {
		t.Fatal("schedule list failed", code, diagnostic.String())
	}
	var page apicontract.SchedulePage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if paused == nil {
		if len(page.Schedules) != 0 {
			t.Fatal("schedule existed before post-start push", page)
		}
		return
	}
	if len(page.Schedules) != 1 {
		t.Fatal("pushed schedule not listed", page, f.trace.String())
	}
	entry := page.Schedules[0]
	if entry.Entry != "nightly" || entry.Paused != *paused {
		t.Fatal("schedule state differs", entry, *paused)
	}
}

func (f *composedFixture) scheduleAccount(t *testing.T) {
	t.Helper()
	f.pipelineAccount(t, gittest.Repository)
}

// pipelineAccount creates the CI pipeline service account of repo.
func (f *composedFixture) pipelineAccount(t *testing.T, repo string) {
	t.Helper()
	name, err := identityname.Name(identityname.Key{Repository: repo}, identityname.PipelineCI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.kube.CoreV1().ServiceAccounts(f.config.PipelineNamespace).Create(t.Context(), &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name}}, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}
