package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// A run whose steps passed and whose publication failed keeps status failed
// (no new statuses) but carries a reason line saying so. A lineage reason the
// run already holds is never replaced: reruns parse it.
func TestFailedPublicationRecordsReasonLine(t *testing.T) {
	for _, tc := range []struct {
		name, prior, failure, want string
		waiting                    bool
	}{
		{name: "permanent", failure: "non-fast-forward",
			want: "steps passed; upstream publication failed; retry with run_rerun, which re-publishes without re-running steps"},
		{name: "exhausted", failure: pushretry.Failure("all 5 attempts exhausted: i/o timeout"),
			want: "steps passed; upstream publication failed after all retry attempts; retry with run_rerun, which re-publishes without re-running steps"},
		{name: "policy wait replaced", waiting: true, failure: "non-fast-forward",
			want: "steps passed; upstream publication failed; retry with run_rerun, which re-publishes without re-running steps"},
		{name: "lineage kept", prior: "rerun of 0123456789abcdef0123456789abcdef: flaky", failure: "non-fast-forward",
			want: "rerun of 0123456789abcdef0123456789abcdef: flaky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
			s := openTestStore(t, t.TempDir()+"/oberth.db", &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/reason", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.prior != "" {
				if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET reason = ? WHERE id = ?`, tc.prior, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.waiting {
				if _, err := s.SetRunPolicyWaiting(t.Context(), run.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			final, err := s.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, tc.failure)
			if err != nil {
				t.Fatal(err)
			}
			if final.Run.Status != model.RunFailed || final.Run.Reason != tc.want || final.Run.Error != tc.failure {
				t.Fatalf("run = status %s reason %q error %q, want reason %q", final.Run.Status, final.Run.Reason, final.Run.Error, tc.want)
			}
		})
	}
}

func TestDeliveredPublicationLeavesReasonEmpty(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := openTestStore(t, t.TempDir()+"/oberth.db", &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/reason", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
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
	final, err := s.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.Run.Status != model.RunPassed || final.Run.Reason != "" {
		t.Fatalf("delivered run = %s %q", final.Run.Status, final.Run.Reason)
	}
}
