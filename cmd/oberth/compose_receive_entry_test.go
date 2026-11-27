//go:build linux

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReviewPreCacheRefusalsMustBeAudited(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	who := f.joinedActor(t)
	for _, shape := range []string{"unregistered", "cancelled", "archived"} {
		t.Run(shape, func(t *testing.T) {
			ctx, repository := f.receiveRefusalCase(t, shape)
			err := f.owners.gitSession(ctx, sshserver.Session{
				Identity: sshserver.Identity{Actor: who.Identity, Fingerprint: who.Fingerprint},
				Command:  sshserver.Command{Service: "git-receive-pack", Repository: repository},
				Input:    io.NopCloser(strings.NewReader("unread push")), Output: io.Discard, Diagnostics: io.Discard,
			})
			if err == nil {
				t.Fatal("expected refusal")
			}
			rows, readErr := f.owners.c.store.AdministrativeAudit(t.Context(), []string{repository}, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
			if readErr != nil || len(rows) != 1 || rows[0].Actor != who.Identity {
				t.Fatalf("authenticated %s receive refused without audit: records=%d, refusal=%v, read_error=%v", shape, len(rows), err, readErr)
			}
		})
	}
}

func (f *composedFixture) receiveRefusalCase(t *testing.T, shape string) (context.Context, string) {
	t.Helper()
	switch shape {
	case "cancelled":
		ctx, stop := context.WithCancel(t.Context())
		stop()
		return ctx, "origin/team/cancelled"
	case "archived":
		repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.owners.c.store.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID); err != nil {
			t.Fatal(err)
		}
		return t.Context(), gittest.Repository
	default:
		return t.Context(), "origin/team/unregistered"
	}
}

func TestComposedReceiveScopeRefusalIsAudited(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	const repository = "outside/team/widget"
	remote := f.git.Git(t, f.git.Work, "remote", "get-url", "oberth")
	f.git.Git(t, f.git.Work, "remote", "set-url", "oberth", strings.ReplaceAll(remote, gittest.Repository, repository))
	root := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git")
	before := cacheTree(t, root)
	if output, err := f.pushResult(t, "refs/heads/probe"); err == nil {
		t.Fatal("expected scope refusal", output, err)
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), []string{repository}, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != 1 || rows[0].Actor != f.joinedActor(t).Identity || rows[0].Details != `{"code":"forbidden"}` {
		t.Fatal("scope refusal lost its audit record", rows, err)
	}
	if !reflect.DeepEqual(before, cacheTree(t, root)) {
		t.Fatal("scope refusal changed the cache")
	}
}

func TestComposedPreCacheStorageRefusalsAreAudited(t *testing.T) {
	binary := composeBinary(t)
	for _, stage := range []string{"initialize", "head"} {
		t.Run(stage, func(t *testing.T) {
			f := composeFixture(t, binary)
			f.prepareCommit(t)
			cancel, done := f.run(t)
			defer cancel()
			f.checkStarting(t, done)
			f.waitStarted(t, done)
			who := f.joinedActor(t)
			restore := f.breakReceiveReference(t, stage)
			defer restore()
			initErr := f.owners.cache.Init(t.Context(), gittest.Repository)
			if stage == "initialize" && initErr == nil {
				t.Fatal("fixture did not fail cache initialization")
			}
			if stage == "head" {
				_, err := f.owners.receiveHead(t.Context(), gittest.Repository)
				if initErr != nil || err == nil {
					t.Fatal("fixture did not isolate head lookup failure", initErr, err)
				}
			}
			err := f.owners.gitSession(t.Context(), sshserver.Session{
				Identity: sshserver.Identity{Actor: who.Identity, Fingerprint: who.Fingerprint},
				Command:  sshserver.Command{Service: "git-receive-pack", Repository: gittest.Repository},
				Input:    io.NopCloser(strings.NewReader("unread push")), Output: io.Discard, Diagnostics: io.Discard,
			})
			rows, readErr := f.owners.c.store.AdministrativeAudit(t.Context(), []string{gittest.Repository}, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
			if err == nil || readErr != nil || len(rows) != 1 || rows[0].Actor != who.Identity {
				t.Fatal("pre-cache storage refusal lost its audit record", stage, rows, err, readErr)
			}
		})
	}
}

func (f *composedFixture) breakReceiveReference(t *testing.T, stage string) func() {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git", "HEAD")
	if stage == "head" {
		path = filepath.Join(filepath.Dir(path), "refs", "heads", "main")
	}
	before, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken reference\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if before == nil {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			return
		}
		if err := os.WriteFile(path, before, 0600); err != nil {
			t.Error(err)
		}
	}
}
