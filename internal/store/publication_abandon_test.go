package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

var publicationAbandonDowngradeSQL = strings.ReplaceAll(publicationAbandonSchema, ", 'abandoned'", "") + `DELETE FROM schema_migrations WHERE version=43;`

func TestPublicationAbandonMigrationRestartAndGuards(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "abandon.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/abandon", strings.Repeat("a", 40)))
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
	pub, err = s.UpdatePublicationAttempt(ctx, pub.ID, 0, 1, time.Now().Add(time.Hour), "upstream publication retry 2/5 queued")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, integrationDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := s.Publication(ctx, pub.ID)
	if err != nil || migrated != pub {
		t.Fatalf("migration changed intent: %+v vs %+v (%v)", migrated, pub, err)
	}
	if _, err = s.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AbandonPublication(ctx, pub.ID, "admin@host", "stop delivery"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.PublicationState(ctx, pub.ID)
	if err != nil || state.Publication.Status != model.PublicationAbandoned || state.Run.Status != model.RunFailed || state.Run.Phase != "abandoned" || state.Run.FinishedAt == nil {
		t.Fatalf("restart changed terminal verdict: %+v %v", state, err)
	}
	if pending, err := s.PendingPublications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("abandoned intent pending after restart: %+v %v", pending, err)
	}
	if _, err = s.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE publications SET status='pending' WHERE id=?`,
		`UPDATE publications SET status='delivered' WHERE id=?`,
		`UPDATE publications SET attempts=attempts+1 WHERE id=?`,
		`DELETE FROM publications WHERE id=?`,
	} {
		if _, err = s.db.ExecContext(ctx, query, pub.ID); err == nil {
			t.Fatalf("terminal mutation accepted: %s", query)
		}
	}
	if _, err = s.RetryFailedPublication(ctx, run.ID, "", "admin@host"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("abandoned retry: %v", err)
	}
}

func TestPublicationAbandonAuditFailureRollsBackOwners(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/abandon-rollback", strings.Repeat("a", 40)))
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
	if _, err = s.db.Exec(`CREATE TRIGGER reject_abandon_audit BEFORE INSERT ON audit_actions WHEN NEW.action='publication.abandon' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AbandonPublication(t.Context(), pub.ID, "admin@host", "stop"); err == nil {
		t.Fatal("abandon committed without audit")
	}
	state, err := s.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationPending || state.Run.Status != model.RunRunning || state.Run.Phase != "publishing" || state.Run.FinishedAt != nil {
		t.Fatalf("partial abandon after audit failure: %+v %v", state, err)
	}
	if _, err = s.VerifyAuditState(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAbandonClosesBothPromotionOwners(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	enqueued, promotion, err := s.EnqueuePromotionRun(t.Context(), model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/abandon",
		SHA: strings.Repeat("b", 40), Actor: "agent@host", Trigger: "promotion", TestedSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40),
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/abandon", SourceSHA: strings.Repeat("b", 40),
		TargetRef: "main", PreviousSHA: strings.Repeat("a", 40), ResultSHA: strings.Repeat("b", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{
		RepoID: repo.ID, RunID: enqueued.ID, PromotionID: promotion.ID,
		RefKind: model.RefBranch, Ref: promotion.TargetRef,
		PreviousSHA: promotion.PreviousSHA, ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.AbandonPublication(t.Context(), pub.ID, "admin@host", "stop")
	if err != nil || final.Publication.Status != model.PublicationAbandoned || final.Run.Status != model.RunFailed || final.Run.Phase != "abandoned" || final.Promotion.Status != model.PromotionFailed {
		t.Fatalf("promotion owners: %+v %v", final, err)
	}
	if _, err = s.VerifyAuditState(t.Context()); err != nil {
		t.Fatal(err)
	}
}
