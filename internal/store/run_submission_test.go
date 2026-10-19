package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunStartsOnlyAfterSubmission(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/waiting", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != enqueued.ID || claimed.StartedAt != nil || claimed.Phase != "waiting" {
		t.Fatalf("claim started run: %#v, %v", claimed, err)
	}
	if _, err := s.MarkRunSubmitted(ctx, claimed.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("submission without Job intent = %v", err)
	}
	if _, err := s.SetRunJobName(ctx, claimed.ID, "oberth-test-workflow"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7 * time.Minute)
	started, err := s.MarkRunSubmitted(ctx, claimed.ID)
	if err != nil || started.Status != model.RunRunning || started.Phase != "running" || started.StartedAt == nil || !started.StartedAt.Equal(now) {
		t.Fatalf("submission start = %#v, %v", started, err)
	}
	if _, err := s.MarkRunSubmitted(ctx, claimed.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate submission start = %v", err)
	}
}
