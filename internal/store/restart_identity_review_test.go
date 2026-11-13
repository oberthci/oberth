package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestTagPublicationUsesRepositoryTagAndSHATuple(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: strings.Repeat("a", 40), Actor: "release", Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	spec := model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefTag, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor}
	first, err := s.BeginPublication(t.Context(), spec)
	requireRestartNoError(t, err)
	again, err := s.BeginPublication(t.Context(), spec)
	if err != nil || again.ID != first.ID {
		t.Fatalf("same release lost its outbox intent: %+v %v", again, err)
	}
	_, err = s.FinalizePublication(t.Context(), first.ID, model.PublicationDelivered, "")
	requireRestartNoError(t, err)
	again, err = s.BeginPublication(t.Context(), spec)
	if err != nil || again.ID != first.ID || again.Status != model.PublicationDelivered {
		t.Fatalf("delivered release lost its identity: %+v %v", again, err)
	}
	duplicate, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: run.Ref, SHA: run.SHA, Actor: run.Actor, Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	spec.RunID = duplicate.ID
	if _, err = s.BeginPublication(t.Context(), spec); !errors.Is(err, ErrInvalidState) {
		t.Fatal("duplicate release acquired another publication", err)
	}

}

func requireRestartNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartMigrationsUpgradeV38WithoutLosingReleaseIntent(t *testing.T) {
	testRestartMigrationsUpgradePreservesReleaseIntent(t, integrationDowngradeSQL, 38)
}

func TestRestartMigrationsUpgradeV43WithoutLosingReleaseIntent(t *testing.T) {
	testRestartMigrationsUpgradePreservesReleaseIntent(t, restartRecoveryDowngradeSQL, 43)
}

func testRestartMigrationsUpgradePreservesReleaseIntent(t *testing.T, downgrade string, version int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v38.db")
	s, err := Open(context.Background(), path, Options{})
	requireRestartNoError(t, err)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v2.0.0", SHA: strings.Repeat("a", 40), Actor: "release", Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	publication, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefTag, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	requireRestartNoError(t, err)
	_, err = s.db.Exec(downgrade)
	requireRestartNoError(t, err)
	var ledgerRows, ledgerVersion int
	requireRestartNoError(t, s.db.QueryRow(`SELECT count(*), max(version) FROM schema_migrations`).Scan(&ledgerRows, &ledgerVersion))
	if ledgerRows != version || ledgerVersion != version {
		t.Fatalf("historical ledger = %d rows through %d, want %d", ledgerRows, ledgerVersion, version)
	}
	requireRestartNoError(t, s.Close())
	upgraded, err := Open(context.Background(), path, Options{})
	requireRestartNoError(t, err)
	defer func() { requireRestartNoError(t, upgraded.Close()) }()
	assertFreshSchemaLedger(t, upgraded.db)
	current, err := upgraded.Run(t.Context(), run.ID)
	requireRestartNoError(t, err)
	if current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence || !current.Release {
		t.Fatal("upgrade changed release identity", current)
	}
	pending, err := upgraded.PendingPublications(t.Context())
	requireRestartNoError(t, err)
	if len(pending) != 1 || pending[0].ID != publication.ID || pending[0].RunID != run.ID {
		t.Fatalf("upgrade changed outbox: %+v", pending)
	}

}
