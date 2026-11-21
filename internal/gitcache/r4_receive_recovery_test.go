package gitcache_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestRecoveryRollsBackUncheckedAndInterruptedRollback(t *testing.T) {
	// The helper releases and reopens cache ownership; isolate it from subprocess startup.
	for _, state := range []string{"reserved", "rejected"} {
		t.Run(state, func(t *testing.T) { checkUncheckedRecovery(t, state) })
	}
}
func checkUncheckedRecovery(t *testing.T, state string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := thirdInit(t, cfg)
	f.Write(t, "file.txt", []byte("unchecked"))
	next := f.CommitFile(t, "unchecked")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	f.Git(t, f.Store(), "update-ref", "refs/heads/main", next)
	f.Git(t, f.Store(), "update-ref", "refs/heads/extra", next)
	if state == "rejected" {
		f.Git(t, f.Store(), "update-ref", "-d", "refs/heads/main")
	}
	record := gitcache.Reservation{State: state, Before: map[string]string{"refs/heads/main": f.Commit},
		Event: gitcache.ReceiveEvent{ID: strings.Repeat("a", 32), Actor: "agent", Fingerprint: "key",
			Repository: gittest.Repository, Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main"}}}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Store(), "oberth-receive.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	called := false
	err = c.ReplayPending(t.Context(), func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		called = true
		return nil, nil
	})
	if err != nil || called {
		t.Fatal("unchecked receive delivered after recovery", err, called)
	}
	got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if got != "refs/heads/main "+f.Commit {
		t.Fatal("unchecked refs survived recovery", got)
	}
}
