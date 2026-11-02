package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

func TestPublicationRetryProgressSurvivesRestartAndAudits(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/retry", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	next := now.Add(2 * time.Minute)
	for i := 0; i < 2; i++ { // replay the same update after a lost commit acknowledgement
		pub, err = s.UpdatePublicationAttempt(ctx, pub.ID, 0, 1, next, "retry 2/5 queued")
		if err != nil || pub.Attempts != 1 {
			t.Fatalf("reservation replay: %+v %v", pub, err)
		}
	}
	if _, err = s.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.PendingPublications(ctx)
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 || !pending[0].RetryAfter.Equal(next) {
		t.Fatalf("restart lost budget: %+v %v", pending, err)
	}
	owner, err := s.Run(ctx, run.ID)
	if err != nil || owner.Error != "retry 2/5 queued" || owner.Phase != "publishing" {
		t.Fatalf("owner: %+v %v", owner, err)
	}
	if _, err = s.UpdatePublicationAttempt(ctx, pub.ID, 1, 6, next, "invalid"); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded retry accepted", err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE publications SET result_sha=? WHERE id=?`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", pub.ID); err == nil {
		t.Fatal("retry allowed changing result SHA")
	}
	if _, err = s.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := s.FinalizePublication(ctx, pub.ID, model.PublicationFailed, pushretry.Failure("retry budget exhausted"))
	if err != nil || final.Run.Phase != "infrastructure" {
		t.Fatalf("final: %+v %v", final, err)
	}
	retried, err := s.RetryFailedPublication(ctx, run.ID, "", "retry@host")
	if err != nil || retried.Attempts != 0 || !retried.RetryAfter.IsZero() {
		t.Fatalf("explicit retry: %+v %v", retried, err)
	}
	if _, err = s.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRetryMigrationPreservesV37PendingIntent(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "v37.db")
	s, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/v37", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(publicationRetryDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE schema_migrations SET applied_at=123 WHERE version=37`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Publication(t.Context(), pub.ID)
	if err != nil || got.ResultSHA != pub.ResultSHA || got.Attempts != 0 || !got.RetryAfter.IsZero() {
		t.Fatalf("migration changed pending intent: %+v %v", got, err)
	}
	var applied int64
	if err = s.db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version=37`).Scan(&applied); err != nil || applied != 123 {
		t.Fatal("v37 ledger was changed", err)
	}
	if _, err = s.VerifyAuditState(t.Context()); err != nil {
		t.Fatal(err)
	}
}
