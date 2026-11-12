package store

import (
	"strings"
	"testing"
	"time"
)

func TestRunPolicyWaitAfterSubmissionIsVisible(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	queuedRunForTest(t, s, repo.ID, "feature/policy-submitted")
	run, err := s.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "policy-workflow"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunSubmitted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	waiting, err := s.SetRunPolicyWaiting(t.Context(), run.ID, true)
	if err != nil || waiting.Phase != "running" || !strings.Contains(waiting.Reason, PolicyRefreshWaitingReason) {
		t.Fatalf("active policy wait was hidden: %#v, %v", waiting, err)
	}
	resumed, err := s.SetRunPolicyWaiting(t.Context(), run.ID, false)
	if err != nil || strings.Contains(resumed.Reason, PolicyRefreshWaitingReason) {
		t.Fatalf("active policy wait was not cleared: %#v, %v", resumed, err)
	}
}
