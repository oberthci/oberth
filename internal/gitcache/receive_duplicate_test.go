package gitcache_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveRepeatedReferenceNeverFencesRepository(t *testing.T) {
	for _, shape := range []string{"write-write", "delete-create"} {
		t.Run(shape, func(t *testing.T) { checkRepeatedReference(t, shape) })
	}
}

func checkRepeatedReference(t *testing.T, shape string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	admissions, deliveries := 0, 0
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { admissions++; return nil }
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/stale", f.Commit)
	f.Write(t, "duplicate.txt", []byte("next"))
	next := f.CommitFile(t, "duplicate")
	updates := []gitcache.RefUpdate{
		{Ref: "refs/heads/stale", OldSHA: f.Commit, NewSHA: next},
		{Ref: "refs/heads/stale", OldSHA: f.Commit, NewSHA: next},
	}
	if shape == "delete-create" {
		updates[0].NewSHA, updates[1].OldSHA = "", ""
	}
	r := fourthAgentPush(t, f, updates, nil)
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { deliveries++; return nil, nil }
	_ = c.Receive(t.Context(), r)
	if admissions != 0 || deliveries != 0 || !strings.Contains(status.String(), "ng refs/heads/stale") {
		t.Fatal("duplicate command reached admission or delivery", admissions, deliveries, status.String())
	}
	if !strings.Contains(feedback.String(), "Git source input is invalid. Use an exact repository, object, ref and clean relative path.") {
		t.Fatal("duplicate refusal is not whole", feedback.String())
	}
	assertStaleReceiveSettled(t, c, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	assertStaleReceiveSettled(t, c, f)
	checkPushAndAbandonAfterStale(t, c, f, next)
}
