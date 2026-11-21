//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

func (f *composedFixture) joinedActor(t *testing.T) actor.Actor {
	t.Helper()
	raw, err := os.ReadFile(f.identity)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	who, err := (currentAuthority{f.owners}).byFingerprint(t.Context(), ssh.FingerprintSHA256(key.PublicKey()), "ssh")
	if err != nil {
		t.Fatal(err)
	}
	return who
}
func (f *composedFixture) pushResult(t *testing.T, ref string, options ...string) (string, error) {
	t.Helper()
	args := append([]string{"-C", f.git.Work, "push"}, options...)
	args = append(args, "oberth", "HEAD:"+ref)
	cmd := exec.CommandContext(t.Context(), f.git.Binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f *composedFixture) checkAuthorityRefusals(t *testing.T) {
	t.Helper()
	who := f.joinedActor(t)
	f.checkCurrentBindings(t, who)
	f.checkAgentAndRelease(t)
	f.checkRevocation(t, who)
}
func (f *composedFixture) checkCurrentBindings(t *testing.T, who actor.Actor) {
	t.Helper()
	stale := who.Clone()
	stale.Identity = "rebound-person"
	if _, err := (currentAuthority{f.owners}).Current(t.Context(), stale); err == nil || !strings.Contains(err.Error(), "Identity binding changed") {
		t.Fatal("stale identity binding accepted", err)
	}
	if _, err := f.owners.receive(t.Context(), gitcache.ReceiveEvent{ID: "changed-actor", Repository: gittest.Repository, Fingerprint: who.Fingerprint, Actor: stale.Identity}, cacheSource{f.owners}); err == nil {
		t.Fatal("receive actor was not checked against current identity")
	}
}
func (f *composedFixture) checkAgentAndRelease(t *testing.T) {
	t.Helper()
	f.git.Write(t, "authority-check", []byte("not on default history"))
	outside := f.git.CommitFile(t, "outside default")
	if output, err := f.pushResult(t, "refs/heads/main"); err == nil || !strings.Contains(output, "default branch requires promotion") {
		t.Fatal("agent moved default branch", output, err)
	}
	f.git.Git(t, f.git.Work, "push", "oberth", "HEAD:refs/heads/outside")
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.owners.ReleaseTag(t.Context(), repo, gitport.Ref{Name: "refs/tags/v-outside", ObjectSHA: outside, CommitSHA: outside}); err == nil || !strings.Contains(err.Error(), "outside default history") {
		t.Fatal("unreachable release commit accepted", err)
	}
}
func (f *composedFixture) checkRevocation(t *testing.T, who actor.Actor) {
	t.Helper()
	if err := f.owners.c.store.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, who.Fingerprint); err != nil {
		t.Fatal(err)
	}
	f.certificateAccess(t, f.joinedCertificate(t), false)
	if _, err := (currentAuthority{f.owners}).byFingerprint(t.Context(), who.Fingerprint, "ssh"); err == nil || !strings.Contains(err.Error(), "Identity is revoked") {
		t.Fatal("revoked identity remains current", err)
	}
	if output, err := f.pushResult(t, "refs/heads/revoked"); err == nil || !strings.Contains(output, "identity was refused") && !strings.Contains(output, "ended the connection before Git began") {
		t.Fatal("revoked key push accepted or misdiagnosed", output, err)
	}
}
