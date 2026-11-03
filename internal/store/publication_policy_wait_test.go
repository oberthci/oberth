package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// The interface assertion keeps the regression compilable on older stores.
// They fail behaviorally because durable policy-wait state is absent.
type publicationPolicyWaitStore interface {
	SetPublicationPolicyWaiting(context.Context, string, bool) (time.Time, error)
}

func TestPublicationPolicyWaitingIsDurableAndResets(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s := openTestStore(t, path, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/policy", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	waits, ok := any(s).(publicationPolicyWaitStore)
	if !ok {
		t.Fatal("store does not persist publication policy waiting")
	}
	now = now.Add(time.Hour) // Age before the first unavailable read is irrelevant.
	first := now
	if since, err := waits.SetPublicationPolicyWaiting(t.Context(), pub.ID, true); err != nil || !since.Equal(first) {
		t.Fatalf("first wait: %v %v", since, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	s = openTestStore(t, path, &now)
	waits = any(s).(publicationPolicyWaitStore)
	if since, err := waits.SetPublicationPolicyWaiting(t.Context(), pub.ID, true); err != nil || !since.Equal(first) {
		t.Fatalf("restart reset first unavailable: %v %v", since, err)
	}
	if since, err := waits.SetPublicationPolicyWaiting(t.Context(), pub.ID, false); err != nil || !since.IsZero() {
		t.Fatalf("clear wait: %v %v", since, err)
	}
	if since, err := waits.SetPublicationPolicyWaiting(t.Context(), pub.ID, true); err != nil || !since.Equal(now) {
		t.Fatalf("new outage reused old origin: %v %v", since, err)
	}
	if _, err := s.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, "permanent refusal"); err != nil {
		t.Fatal(err)
	}
	if _, err := waits.SetPublicationPolicyWaiting(t.Context(), pub.ID, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("terminal publication accepted waiting: %v", err)
	}
	if _, err := waits.SetPublicationPolicyWaiting(t.Context(), "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing publication accepted waiting: %v", err)
	}
}
