package gitcache_test

import (
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryDeletesExistingChildrenBeforeCreatingParent(t *testing.T) {
	t.Parallel()
	for _, names := range [][]string{{"topic/x", "topic"}, {"topic", "topic/x"}} {
		t.Run(names[0], func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			c := thirdInit(t, cfg)
			old, next := "refs/heads/"+names[0], "refs/heads/"+names[1]
			f.Git(t, f.Store(), "update-ref", old, f.Commit)
			f.Git(t, f.Store(), "update-ref", "refs/oberth/upstream/heads/"+names[1], f.Commit)
			marker := filepath.Join(f.Store(), "oberth-reconcile")
			if err := os.WriteFile(marker, []byte(old+"\n"+next+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.ResolveRef(t.Context(), gittest.Repository, next); err != nil {
				t.Fatal("recovery stuck on directory rename", err)
			}
			if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", old); got == old {
				t.Fatal("old ref survived", got)
			}
		})
	}
}
func TestConflictingMarkerIsSetAsideWithoutDeletingLocalHead(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	logs := 0
	cfg.Log = func(reason, detail string) {
		if reason == "stuck mirror marker set aside" && strings.Contains(detail, "refs/heads/topic") {
			logs++
		}
	}
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic/local", f.Commit)
	f.Git(t, f.Store(), "update-ref", "refs/oberth/upstream/heads/topic", f.Commit)
	marker := filepath.Join(f.Store(), "oberth-reconcile")
	if err := os.WriteFile(marker, []byte("refs/heads/topic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := thirdServes(t.Context(), c, f); err != nil {
		t.Fatal("stuck marker blocked reads", err)
	}
	if _, err := os.Stat(marker + ".stuck"); err != nil || logs != 1 {
		t.Fatal("marker not set aside", err, logs)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic/local"); got != f.Commit {
		t.Fatal("local head changed", got)
	}
}
