//go:build linux

// This test exercises the Linux server and Git receive path.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

func TestAdministratorTopicPublicationRefusesDeletedDefault(t *testing.T) {
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
	forgeRefs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname) %(objectname)")
	f.git.Write(t, "topic.txt", []byte("new topic\n"))
	f.git.CommitFile(t, "topic after default deletion")
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	gittest.ExpireRefresh(t, store)
	// A cached topic receive cannot discover a later forge deletion. The live
	// publication check must still refuse it, including an administrator's run.
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/topic"); err != nil {
		t.Fatal("cached topic push refused before CI", err, output)
	}
	if f.forge.sessions.Load() != before {
		t.Fatal("cached topic receive consulted the forge")
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.sourcePod(t, run)
	f.finishNative(t, run)
	publication := f.waitPublicationSettled(t, "publication:"+run.ID)
	if publication.Status != model.PublicationFailed || !strings.Contains(publication.Detail, "upstream default branch was deleted") {
		t.Fatal("topic publication did not refuse the deleted default", publication)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); refs != "" {
		t.Fatal("refused topic reached the forge", refs)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname) %(objectname)"); refs != forgeRefs {
		t.Fatal("topic publication changed existing forge history", refs)
	}
}
