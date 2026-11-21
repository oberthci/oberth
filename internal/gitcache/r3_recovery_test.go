package gitcache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestStuckMarkerIsSetAsideAndRepositoryServes(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	logs := 0
	cfg.Log = func(reason, detail string) {
		if reason == "stuck mirror marker set aside" && strings.Contains(detail, gittest.Repository) {
			logs++
		}
	}
	c := thirdInit(t, cfg)
	other := "origin/team/other"
	if err := c.Init(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	marker := filepath.Join(f.Store(), "oberth-reconcile")
	if err := os.WriteFile(marker, []byte("refs/private/corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	for _, repo := range []string{gittest.Repository, other} {
		if _, err := c.ResolveRef(t.Context(), repo, "refs/heads/main"); err != nil {
			t.Fatal("repository cannot serve", err)
		}
	}
	if logs != 1 {
		t.Fatal("stuck marker observation", logs)
	}
	if _, err := os.Stat(marker + ".stuck"); err != nil {
		t.Fatal("marker not preserved", err)
	}
	if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("fetch after marker quarantine", err)
	}
}

func TestScaleRecoveryAcceptsTwoFullLongNameInventoriesRefusesOneMoreName(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	var tx, marker strings.Builder
	tx.WriteString("delete refs/oberth/upstream/heads/main\n")
	for i := 0; i < 2*sourcecontract.MaxRefs; i++ {
		// The suffix is the contract's maximum 255 bytes, with legal disk components.
		name := fmt.Sprintf("refs/heads/%04d%s/%s", i, strings.Repeat("x", 95), strings.Repeat("y", 155))
		fmt.Fprintf(&tx, "update refs/oberth/upstream/heads/%s %s\n", strings.TrimPrefix(name, "refs/heads/"), f.Commit)
		fmt.Fprintln(&marker, name)
	}
	r2RefTransaction(t, f.Store(), tx.String())
	path := filepath.Join(f.Store(), "oberth-reconcile")
	if err := os.WriteFile(path, []byte(marker.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal("valid two-inventory recovery", err)
	}
	if err := os.WriteFile(path, []byte(marker.String()+"refs/heads/extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".stuck"); err != nil {
		t.Fatal("oversized marker not quarantined", err)
	}
	if err := os.Remove(path + ".stuck"); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/oberth/upstream/heads/extra", f.Commit)
	if err := os.WriteFile(path, []byte("refs/heads/extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err == nil {
		t.Fatal("accepted too many partial tracking refs")
	}
	f.Git(t, f.Store(), "update-ref", "-d", "refs/oberth/upstream/heads/extra")
	if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("next fetch cannot consume a full partial generation", err)
	}
	got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/upstream/")
	if got != "refs/oberth/upstream/heads/main" {
		t.Fatal("partial tracking survived refresh", got)
	}
}

func TestScalePartialGenerationAndAnotherSnapshotKeepRecoveryBounded(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	wrapper := filepath.Join(f.Root, "partial-git")
	script := "#!/bin/sh\ncase \" $* \" in\n*' update-ref '*' --no-deref '*) echo $$ > " +
		strconv.Quote(filepath.Join(f.Root, "stopped")) + "; kill -STOP $$;;\nesac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	var before, after strings.Builder
	// A reachable mixed generation from two 2050-ref snapshots sharing main.
	for i := 0; i < 4098; i++ {
		fmt.Fprintf(&before, "update refs/heads/old%04d %s\n", i, f.Commit)
		fmt.Fprintf(&before, "update refs/oberth/upstream/heads/old%04d %s\n", i, f.Commit)
	}
	for i := 0; i < 4095; i++ {
		fmt.Fprintf(&after, "update refs/heads/new%04d %s\n", i, f.Commit)
	}
	r2RefTransaction(t, f.Store(), before.String())
	r2RefTransaction(t, f.Bare, after.String())
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	waitMirrorStop(t, f, done)
	data, err := os.ReadFile(filepath.Join(f.Store(), "oberth-reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	// Always terminate and join our stopped child before asserting the bound.
	reviewKill(t, f)
	if err = <-done; err == nil {
		t.Fatal("killed mirror succeeded")
	}
	if count := len(strings.Fields(string(data))); count > 2*sourcecontract.MaxRefs {
		t.Fatalf("third inventory enlarged the recovery marker to %d names", count)
	}
	_ = c.Close()
	cfg.Binary = f.Binary
	c = reviewNew(t, cfg)
	if err = c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("retry after second interruption", err)
	}
	got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/upstream/")
	if len(strings.Fields(got)) != sourcecontract.MaxRefs || strings.Contains(got, "/old") {
		t.Fatal("tracking did not converge to the admitted snapshot")
	}
}

func TestFetchIncomingSnapshotAccepts4096Refuses4097(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	var seed strings.Builder
	for i := 0; i < sourcecontract.MaxRefs-1; i++ {
		fmt.Fprintf(&seed, "update refs/heads/branch%04d %s\n", i, f.Commit)
	}
	r2RefTransaction(t, f.Bare, seed.String())
	c := f.Cache(t, nil)
	if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("N", err)
	}
	f.Git(t, f.Bare, "update-ref", "refs/heads/overflow", f.Commit)
	if err := c.ForceFetch(t.Context(), gittest.Repository); err == nil {
		t.Fatal("N+1 snapshot was admitted")
	}
	refs, err := c.ListRefs(t.Context(), gittest.Repository)
	if err != nil || len(refs) != sourcecontract.MaxRefs {
		t.Fatal("overflow changed accepted inventory", len(refs), err)
	}
}
