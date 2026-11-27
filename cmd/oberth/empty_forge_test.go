//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"golang.org/x/crypto/ssh"
)

func TestEmptyForgeBindsAndPublishesFirstMainPush(t *testing.T) {
	f, run := firstEmptyForgePush(t)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	f.waitPublication(t, "publication:"+run.ID)
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != *run.CommitSHA {
		t.Fatal("forge lacks first main commit", head)
	}
}

func TestEmptyForgeFirstPublicationPreservesExternalMain(t *testing.T) {
	f, run := firstEmptyForgePush(t)
	tree := f.git.Git(t, f.git.Work, "rev-parse", "HEAD^{tree}")
	external := f.git.Git(t, f.git.Work, "commit-tree", tree, "-m", "independent forge history")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, external+":refs/heads/main")
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	publication := f.waitPublicationSettled(t, "publication:"+run.ID)
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != external {
		t.Fatal("first publication overwrote independent forge history", head, external)
	}
	if publication.Status != model.PublicationFailed && publication.Status != model.PublicationAbandoned {
		t.Fatal("first publication did not refuse the new forge head", publication)
	}
	if publication.PreviousSHA == nil || *publication.PreviousSHA != external {
		t.Fatal("first publication did not read the live forge head", publication)
	}
}

func firstEmptyForgePush(t *testing.T) (*composedFixture, stateport.Run) {
	t.Helper()
	f := composeFixture(t, composeBinary(t))
	freshRepoAddCache(f)
	f.git.Bare = filepath.Join(t.TempDir(), "empty.git")
	f.git.Git(t, f.git.Root, "init", "--bare", "--initial-branch=main", f.git.Bare)
	f.git.Write(t, ".oberth/build.yaml", []byte(composedWorkflow))
	commit := f.git.CommitFile(t, "first pipeline")
	f.prepareAdministrator(t)
	identity, public := composeSSHKey(t, t.TempDir(), "administrator")
	f.identity = identity
	if err := os.WriteFile(f.config.Files.AdminPushKey, ssh.MarshalAuthorizedKey(public), 0600); err != nil {
		t.Fatal(err)
	}
	close(f.allowList)
	cancel, done := f.run(t)
	t.Cleanup(cancel)
	f.waitStarted(t, done)
	command := newRepoAddCommand(t, f)
	if code := command.run(t, "--upstream", "origin", "--expected-host-fingerprint", ssh.FingerprintSHA256(f.forge.key)); code != 0 {
		t.Fatal("empty forge did not bind", code, command.diagnostic.String())
	}
	if _, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil); err != nil {
		t.Fatal("repository was not registered", err)
	}
	f.checkEmptyForgeAuthority(t)
	f.identity = identity
	f.bindClient(t)
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	gittest.ExpireRefresh(t, store)
	if output, err := f.pushResult(t, "refs/heads/main"); err != nil {
		t.Fatal("first main push refused", err, output)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.CommitSHA == nil || *run.CommitSHA != commit || run.Ref != "refs/heads/main" {
		t.Fatal("first push did not follow ordinary admission", run.RunSummary)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("first push published before passing CI", refs)
	}
	f.sourcePod(t, run)
	return f, run
}

func (f *composedFixture) checkEmptyForgeAuthority(t *testing.T) {
	t.Helper()
	a := serverAdministration{f.owners}
	restoreHost := f.replaceForgeHost(t)
	if _, err := a.ProbeUpstream(t.Context(), "origin", "team/widget"); !refusedWith(err, gitssh.HostKeyMismatchReason) {
		t.Fatal("empty forge hid host pin refusal", err)
	}
	restoreHost()
	original := f.forge.config.Load()
	denied := *original
	denied.NoClientAuth = false
	denied.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, errors.New("forge access refused")
	}
	f.forge.config.Store(&denied)
	_, err := a.ProbeUpstream(t.Context(), "origin", "team/widget")
	f.forge.config.Store(original)
	if !refusedWith(err, gitport.ForgeReason) {
		t.Fatal("empty forge hid refused access", err)
	}
	f.enrollJoined(t)
	f.bindClient(t)
	if output, err := f.pushResult(t, "refs/heads/main"); err == nil || !strings.Contains(output, "default branch requires promotion") {
		t.Fatal("agent could seed the default branch", output, err)
	}
}

func (f *composedFixture) waitPublication(t *testing.T, id string) {
	t.Helper()
	if p := f.waitPublicationSettled(t, id); p.Status != model.PublicationDelivered {
		t.Fatal("publication did not reach forge", p, f.trace.String())
	}
}

func (f *composedFixture) waitPublicationSettled(t *testing.T, id string) model.Publication {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		changes := observeChanges(t, f.owners.c.store)
		p, err := f.owners.c.store.Publication(t.Context(), id)
		if err == nil && p.Status != model.PublicationPending {
			return p
		}
		select {
		case <-changes:
		case <-deadline.C:
			t.Fatal("publication did not settle", p, err, f.trace.String())
		}
	}
}
