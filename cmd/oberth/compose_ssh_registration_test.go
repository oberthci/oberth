//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedSSHRefusesUnregisteredWithoutWriting(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	who := f.joinedActor(t)
	root := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git")
	before := cacheTree(t, root)
	audit, head, err := f.owners.c.store.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := f.owners.c.store.Repositories(t.Context(), stateport.CatalogFilter{Limit: 200, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"git-upload-pack", "git-receive-pack"} {
		for _, repo := range []string{"origin/team/unregistered", "origin/other/widget", "unknown/team/widget"} {
			err := f.owners.gitSession(t.Context(), sshserver.Session{Identity: sshserver.Identity{Fingerprint: who.Fingerprint}, Command: sshserver.Command{Service: verb, Repository: repo}, Input: io.NopCloser(strings.NewReader("unread push")), Output: io.Discard})
			assertRepositoryNotFound(t, err)
		}
	}
	afterAudit, afterHead, err := f.owners.c.store.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	afterRepos, err := f.owners.c.store.Repositories(t.Context(), stateport.CatalogFilter{Limit: 200, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, cacheTree(t, root)) || !reflect.DeepEqual(repos, afterRepos) {
		t.Fatal("unregistered SSH request changed cache or repositories")
	}
	if len(afterAudit) != len(audit)+3 || afterHead.ID != head.ID+3 || !reflect.DeepEqual(audit, afterAudit[:len(audit)]) {
		t.Fatal("unregistered receive refusals did not append exactly three audit records", afterAudit, afterHead)
	}
	for _, row := range afterAudit[len(audit):] {
		if row.Action != "receive.refused" || row.Actor != who.Identity || row.ResourceType != "repository" {
			t.Fatal("unregistered refusal lost trusted attribution", row)
		}
	}
}

func assertRepositoryNotFound(t *testing.T, err error) {
	t.Helper()
	problem := publicerr.Classify(err)
	if problem.Code() != publicerr.NotFound || problem.Reason() != "Repository not found." ||
		!strings.Contains(problem.Remedy(), "not registered here") || !strings.Contains(problem.Remedy(), "oberth repo list") {
		t.Fatal("unregistered repository lost its listing remedy", err)
	}
}

func cacheTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			files[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		files[path] = fmt.Sprintf("%x", sha256.Sum256(data))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
