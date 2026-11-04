package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestManualRerunPreservesReasonAndAutomaticLineage(t *testing.T) {
	for _, mode := range []string{"child", "original", "cancelled", "legacy", "omitted-reason"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			origin := autoRetrySource(t, s, repo.ID, "feature/reason")
			failed, err := s.FinishRun(t.Context(), origin.ID, autoRetryResult())
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.FinishRun(t.Context(), failed.AutoRetriedBy, autoRetryResult())
			if err != nil {
				t.Fatal(err)
			}
			source := child
			switch mode {
			case "original":
				source = failed
			case "legacy":
				if _, err = s.db.Exec(`UPDATE runs SET reason=? WHERE id=?`, legacyAutoRetryOfPrefix+origin.ID, child.ID); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if _, err = s.db.Exec(`UPDATE runs SET status='interrupted',phase='cancelled',reason='operator cancelled' WHERE id=?`, child.ID); err != nil {
					t.Fatal(err)
				}
			}
			reason := "rerun after repairing the fixture (lineage: automatic retry of " + strings.Repeat("f", 32) + ")"
			if mode == "omitted-reason" {
				reason = ""
			}
			result, err := s.RerunRun(t.Context(), model.RunRerunRequest{RunID: source.ID, Actor: "operator", Reason: reason})
			if err != nil {
				t.Fatal(err)
			}
			want := "rerun of " + source.ID + " (lineage: automatic retry of " + origin.ID + ")"
			if reason != "" {
				want += ": " + reason
			}
			check := func(run model.Run) {
				t.Helper()
				if run.Reason != want || run.AutoRetryOf != origin.ID {
					t.Fatalf("manual reason/lineage: got %q, origin %q; want %q, origin %q", run.Reason, run.AutoRetryOf, want, origin.ID)
				}
			}
			check(result.Enqueued.Run)
			action, err := s.LatestAuditActionByType(t.Context(), "run.rerun", "run")
			var details map[string]any
			if err != nil || json.Unmarshal([]byte(action.Details), &details) != nil || details["reason"] != reason || details["source_run_id"] != source.ID || action.Actor != "operator" {
				t.Fatalf("operator audit: %+v %v", action, err)
			}
			lineage, err := s.LatestAuditActionByType(t.Context(), "run.auto-retry-lineage", "run")
			if err != nil || lineage.ResourceID != result.Enqueued.ID || !strings.Contains(lineage.Details, origin.ID) {
				t.Fatalf("lineage audit: %+v %v", lineage, err)
			}
			path := s.livePath
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenAdminClient(t.Context(), path, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			reread, err := s.Run(t.Context(), result.Enqueued.ID)
			if err != nil {
				t.Fatal(err)
			}
			check(reread)
			claimed, err := s.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			check(claimed)
			requeued, err := s.RequeueStrandedRun(t.Context(), claimed.ID)
			if err != nil || requeued.AutoRetryOf != origin.ID || !strings.Contains(requeued.Reason, want) {
				t.Fatalf("restart lost manual reason/lineage: %+v %v", requeued, err)
			}
			terminal, err := s.FinishRun(t.Context(), requeued.ID, autoRetryResult())
			if err != nil || terminal.AutoRetriedBy != "" || terminal.AutoRetryOf != origin.ID {
				t.Fatalf("manual rerun reset automatic budget: %+v %v", terminal, err)
			}
		})
	}
}
