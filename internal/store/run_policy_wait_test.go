package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunPolicyWaitPreservesRetryLineageAndCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/policy")
	failed, err := s.FinishRun(t.Context(), source.ID, autoRetryResult())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(automaticRetryDelay)
	run, err := s.ClaimNextRun(t.Context())
	if err != nil || run.ID != failed.AutoRetriedBy {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	original := run.Reason
	for range 2 {
		waiting, err := s.SetRunPolicyWaiting(t.Context(), run.ID, true)
		if err != nil || waiting.AutoRetryOf != source.ID || strings.Count(waiting.Reason, PolicyRefreshWaitingReason) != 1 {
			t.Fatalf("waiting lost lineage: %#v, %v", waiting, err)
		}
	}
	resumed, err := s.SetRunPolicyWaiting(t.Context(), run.ID, false)
	if err != nil || resumed.AutoRetryOf != source.ID || resumed.Reason != original {
		t.Fatalf("resumed lost lineage: %#v, %v", resumed, err)
	}
	if _, err := s.SetRunPolicyWaiting(t.Context(), run.ID, true); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "owner", Reason: "stop waiting"})
	if err != nil {
		t.Fatal(err)
	}
	for _, waiting := range []bool{true, false} {
		current, err := s.SetRunPolicyWaiting(t.Context(), run.ID, waiting)
		if err != nil || current.Status != model.RunInterrupted || current.Reason != cancelled.Run.Reason {
			t.Fatalf("policy wait overwrote cancellation: %#v, %v", current, err)
		}
	}
}
