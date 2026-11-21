package gitcache_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestAbandonRestoresUnpreparedAfterFailedStartupRecovery(t *testing.T) {
	for _, state := range []string{"reserved", "ready", "ready-empty"} {
		t.Run(state, func(t *testing.T) { checkUnpreparedAbandon(t, state) })
	}
}

func checkUnpreparedAbandon(t *testing.T, state string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	record, next := pendingAbandonReceive(t, c, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	record.State, record.Prepared = state, nil
	if state == "ready-empty" {
		record.State, record.Prepared = "ready", map[string]struct{}{}
	}
	writeReceiveEvidence(t, f, record)
	repair := blockAbandonStorage(t, f)
	c = reviewNew(t, cfg)
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.State != record.State || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != next {
		t.Fatal("startup did not leave the obstructed unprepared receive", pending, err)
	}
	repair()
	if err = c.AbandonReceive(t.Context(), gittest.Repository, record.Event.ID); err != nil {
		t.Fatal(err)
	}
	if completionState(t, f) != "absent" || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != f.Commit {
		t.Fatal("abandon discarded an unprepared receive without restoring it")
	}
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("abandoned unprepared receive delivered")
		return nil, nil
	})
	pushAfterAbandon(t, c, f, f.Commit)
}

func writeReceiveEvidence(t *testing.T, f *gittest.Fixture, record gitcache.Reservation) {
	t.Helper()
	data, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(filepath.Join(f.Store(), "oberth-receive.json"), data, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func blockAbandonStorage(t *testing.T, f *gittest.Fixture) func() {
	t.Helper()
	dirs := []string{f.Store(), filepath.Join(f.Store(), "refs", "heads")}
	for _, dir := range dirs {
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, dir := range dirs {
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAbandonUnpreparedAfterNativeStopAndFaultyRestart(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "unprepared.txt", []byte("unprepared"))
	next := f.CommitFile(t, "unprepared")
	input := receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: next})
	data, err := io.ReadAll(input)
	_ = input.Close()
	if err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(f.Root, "unprepared.pack")
	if err = os.WriteFile(pack, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	runPreparationStop(t, preparationStop{Root: cfg.Root, Scratch: cfg.Scratch, Binary: cfg.Binary, Hook: cfg.HookBinary,
		Store: f.Store(), Pack: pack, Commit: f.Commit, Boundary: "transport", Damage: "bypassed"})
	record := readCompletionRecord(t, f)
	if record.State != "ready" || record.Prepared != nil {
		t.Fatal("stopped native receive was prepared", record)
	}
	repair := blockAbandonStorage(t, f)
	c = reopenStoppedReceive(t, cfg)
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.Event.ID != record.Event.ID || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != next {
		t.Fatal("faulty restart did not preserve the moved ref and record", pending, err)
	}
	repair()
	if err = c.AbandonReceive(t.Context(), gittest.Repository, record.Event.ID); err != nil {
		t.Fatal(err)
	}
	if completionState(t, f) != "absent" || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != f.Commit {
		t.Fatal("first-operation abandon left the unprepared native update")
	}
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("abandoned native update delivered")
		return nil, nil
	})
	pushAfterAbandon(t, c, f, f.Commit)
}
