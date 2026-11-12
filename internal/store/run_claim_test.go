package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestMatchingClaimSkipsPagesAndRechecksCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	var runs []model.Run
	for i := 0; i < 66; i++ {
		result, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, fmt.Sprintf("branch-%d", i), strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, result.Run)
	}
	got, err := s.ClaimNextRunMatching(t.Context(), func(run model.Run) bool {
		// A nested routing read must work even with bounded reader connections.
		if _, err := s.Run(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		return run.ID == runs[65].ID
	})
	if err != nil || got.ID != runs[65].ID {
		t.Fatal(got, err)
	}
	for _, run := range runs[:65] {
		current, err := s.Run(t.Context(), run.ID)
		if err != nil || current.Status != model.RunQueued {
			t.Fatal("skipped run was claimed", current, err)
		}
	}
	_, err = s.ClaimNextRunMatching(t.Context(), func(run model.Run) bool {
		if run.ID != runs[0].ID {
			return false
		}
		if _, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "admin", Reason: "cancel during selection"}); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("selected cancellation bypassed claim guard", err)
	}
}
