//go:build linux

// This test exercises the Linux server and Git receive path.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

func TestAdministratorTopicPushRefusesDeletedDefault(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	identity, public := composeSSHKey(t, t.TempDir(), "administrator")
	if err := os.WriteFile(f.config.Files.AdminPushKey, ssh.MarshalAuthorizedKey(public), 0600); err != nil {
		t.Fatal(err)
	}
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	f.identity = identity
	f.bindClient(t)
	if _, err := f.owners.cache.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal("default branch was not cached", err)
	}
	f.git.Git(t, f.git.Bare, "update-ref", "refs/heads/existing", "refs/heads/main")
	f.git.Git(t, f.git.Bare, "update-ref", "-d", "refs/heads/main")
	f.git.Write(t, "topic.txt", []byte("new topic\n"))
	f.git.CommitFile(t, "topic after default deletion")
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	gittest.ExpireRefresh(t, store)
	if output, err := f.pushResult(t, "refs/heads/topic"); err == nil || !strings.Contains(output, "upstream default branch was deleted") {
		t.Fatal("topic push admitted without a default head", err, output)
	}
	if refs := f.git.Git(t, store, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); refs != "" {
		t.Fatal("refused topic was accepted", refs)
	}
	if runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 2}); err != nil || len(runs) != 0 {
		t.Fatal("refused topic started CI", runs, err)
	}
}
