package gitcache_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestUnsettledReceiveNamesWorkingManualRepair(t *testing.T) {
	for _, shape := range []string{"outside-writer", "unprepared-parent"} {
		t.Run(shape, func(t *testing.T) { checkManualReceiveRepair(t, shape) })
	}
}

func checkManualReceiveRepair(t *testing.T, shape string) {
	t.Helper()
	f, c, record := manualReceiveRepair(t, shape)
	_, readErr := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main")
	deliver := func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("unsettled receive delivered")
		return nil, nil
	}
	failures := []error{c.RetryReceive(t.Context(), gittest.Repository, deliver), c.AbandonReceive(t.Context(), gittest.Repository, record.Event.ID)}
	if shape == "outside-writer" {
		if readErr != nil {
			t.Fatal("outside writer fenced reads", readErr)
		}
		checkMismatchReads(t, f, c)
	} else {
		failures = append(failures, readErr)
	}
	for _, err := range failures {
		var feedback interface{ GitFeedback() string }
		want := "receive refs need manual repair. Have the operator repair storage or restore the pending receive's before refs manually; then retry or abandon."
		if !errors.As(err, &feedback) || feedback.GitFeedback() != want {
			t.Fatal("manual repair has no whole working remedy", err)
		}
	}
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.Event.ID != record.Event.ID {
		t.Fatal("manual evidence is not inspectable", err)
	}
	if shape == "unprepared-parent" {
		f.Git(t, f.Store(), "update-ref", "-d", "refs/heads/topic/leaf")
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", pending.Before["refs/heads/topic"])
	if err = c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
		t.Fatal("documented manual repair did not unblock abandon", err)
	}
	retryCompletion(t, c, deliver)
	pushAfterAbandon(t, c, f, f.Commit)
}

func manualReceiveRepair(t *testing.T, shape string) (*gittest.Fixture, *gitcache.Cache, gitcache.Reservation) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	record, next := pendingAbandonReceive(t, c, f)
	if shape == "outside-writer" {
		f.Write(t, "outside.txt", []byte("outside"))
		third := f.CommitFile(t, "outside")
		f.Git(t, f.Store(), "fetch", f.Work, third)
		f.Git(t, f.Store(), "update-ref", "refs/heads/topic", third)
	} else {
		f.Git(t, f.Store(), "update-ref", "-d", "refs/heads/topic")
		f.Git(t, f.Store(), "update-ref", "refs/heads/topic/leaf", next)
		record.Event.Updates = []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit}, {Ref: "refs/heads/topic/leaf", NewSHA: next}}
		record.Prepared = map[string]struct{}{"refs/heads/topic/leaf": {}}
		writeReceiveEvidence(t, f, record)
	}
	return f, c, record
}

func checkMismatchReads(t *testing.T, f *gittest.Fixture, c *gitcache.Cache) {
	t.Helper()
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.CommitSHA != f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") {
		t.Fatal("moved ref is not readable", ref, err)
	}
	file, err := c.ReadFile(t.Context(), gitport.Revision{Repository: gittest.Repository, Commit: ref.CommitSHA}, "outside.txt")
	if err != nil || string(file.Bytes) != "outside" {
		t.Fatal("moved source is not readable", file, err)
	}
	if _, err = c.ListRefs(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	if err = c.Upload(t.Context(), gittest.Repository, gitcache.ServiceIO{Input: io.NopCloser(strings.NewReader("0000")), Output: &output}); err != nil || !strings.Contains(output.String(), "refs/heads/topic") {
		t.Fatal("mismatched receive blocked upload", err, output.String())
	}
	staged, err := c.Stage(t.Context(), f.Bindings(f.Commit)[0])
	if err != nil {
		t.Fatal("mismatched receive blocked exact source delivery", err)
	}
	if err = staged.Body.Close(); err != nil {
		t.Fatal(err)
	}
}
