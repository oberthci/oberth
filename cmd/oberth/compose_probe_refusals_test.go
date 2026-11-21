//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

func TestComposedProbeReportsChangedHostKeyWithoutStagingPaths(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	a := serverAdministration{f.owners}
	restoreHost := f.replaceForgeHost(t)
	out, err := a.ProbeUpstream(t.Context(), "origin", "team/widget")
	if err != nil || !out.Pending || out.DisplayReason != gitssh.HostKeyMismatchReason {
		t.Fatal("host-key change was not identified", out, err)
	}
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = a.ProbeRepository(t.Context(), repo)
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Reason() != gitssh.HostKeyMismatchReason || problem.Detail != "" || strings.Contains(problem.Error(), f.config.DatabaseFile) {
		t.Fatal("unsafe or incorrect probe refusal", err)
	}
	restoreHost()
	f.git.Git(t, f.git.Bare, "symbolic-ref", "HEAD", "refs/heads/absent")
	out, err = a.ProbeUpstream(t.Context(), "origin", "team/widget")
	if err != nil || !out.Pending || out.DisplayReason != "Repository check failed." {
		t.Fatal("unavailable repository was reported as changed trust", out, err)
	}
}

func (f *composedFixture) replaceForgeHost(t *testing.T) func() {
	t.Helper()
	original := f.forge.config.Load()
	path, _ := composeSSHKey(t, t.TempDir(), "changed-host")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	changed := &ssh.ServerConfig{NoClientAuth: true}
	changed.AddHostKey(signer)
	f.forge.config.Store(changed)
	return func() { f.forge.config.Store(original) }
}
