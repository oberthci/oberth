package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

func TestPurgeCancellationDoesNotWaitForSweep(t *testing.T) {
	f := newRetentionFixture(t)
	f.retention.mu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.retention.Purge(ctx, api.Actor{Identity: "admin", Admin: true}, "aaaaaaaa", "logs", "test")
		done <- err
	}()
	select {
	case err := <-done:
		f.retention.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		f.retention.mu.Unlock()
		<-done
		t.Fatal("canceled purge waited for the sweep mutex")
	}
}

func TestManualPurgeDoesNotWaitForUnrelatedSweep(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/purge", "branch", true)
	f.retention.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.retention.Purge(ctx, api.Actor{Identity: "admin", Admin: true}, run.ID, "logs", "manual cleanup")
		done <- err
	}()
	select {
	case err := <-done:
		f.retention.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		f.retention.mu.Unlock()
		<-done
		t.Fatal("manual purge waited for an unrelated sweep")
	}
}

func TestSweepCandidateAlreadyPurgedManuallyAddsNoAudit(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/purge-race", "branch", true)
	actor := api.Actor{Identity: "owner", Admin: true}
	if _, err := f.retention.Purge(t.Context(), actor, run.ID, "logs", "manual cleanup"); err != nil {
		t.Fatal(err)
	}
	before, err := f.retention.Database.VerifyAuditState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A sweep may already have selected this candidate before the manual purge.
	value, err := f.retention.purgeSweepCandidate(t.Context(), run.ID, "logs", "retention policy", 0)
	if err != nil || value.Bytes != 0 {
		t.Fatalf("stale sweep candidate: %+v %v", value, err)
	}
	after, err := f.retention.Database.VerifyAuditState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("stale sweep duplicated audit: %+v -> %+v", before, after)
	}
}
