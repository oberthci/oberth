package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func rotationAuditSpec(t *testing.T, fields map[string]string, action string) model.AuditActionSpec {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return model.AuditActionSpec{Actor: "scheduler", Action: "trusted.pin_rotation." + action,
		ResourceType: "repository", ResourceID: "7", Details: string(body)}
}

func rotationAuditFields() map[string]string {
	return map[string]string{
		"run_id": "run-1", "candidate_sha": strings.Repeat("e", 40), "baseline_sha": strings.Repeat("f", 40),
		"from_suite_sha256": strings.Repeat("a", 64), "from_guest_sha256": strings.Repeat("b", 64),
		"to_suite_sha256": strings.Repeat("c", 64), "to_guest_sha256": strings.Repeat("d", 64),
	}
}

func TestPinRotationAuditTransitionsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.db")
	database, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	fields := rotationAuditFields()
	appendDecision := func(action string, wantID int64) {
		t.Helper()
		record, err := database.AppendAuditAction(ctx, rotationAuditSpec(t, fields, action))
		if err != nil || record.ID != wantID {
			t.Fatalf("%s audit ID=%d err=%v, want %d", action, record.ID, err, wantID)
		}
	}
	for range 4 {
		appendDecision("authorized", 1)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A new default commit with the same pins is still the same decision.
	fields["baseline_sha"] = strings.Repeat("1", 40)
	appendDecision("authorized", 1)
	fields["from_suite_sha256"], fields["from_guest_sha256"] = strings.Repeat("2", 64), strings.Repeat("3", 64)
	appendDecision("authorized", 2)
	fields["reason"] = "recipe removed"
	appendDecision("refused", 3)
	fields["reason"] = "recipe still absent"
	appendDecision("refused", 3)
	delete(fields, "reason")
	appendDecision("authorized", 4)
	fields["run_id"] = "run-2"
	appendDecision("authorized", 5)
	fields["run_id"] = "run-1"
	spec := rotationAuditSpec(t, fields, "authorized")
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			record, err := database.AppendAuditAction(ctx, spec)
			if err != nil || record.ID != 4 {
				t.Errorf("concurrent recheck: ID=%d err=%v", record.ID, err)
			}
		})
	}
	group.Wait()
	// A failed append must not remember a decision that never reached the chain.
	if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER reject_rotation BEFORE INSERT ON audit_actions BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, rotationAuditSpec(t, fields, "refused")); err == nil {
		t.Fatal("rotation progressed after its new audit record failed")
	}
	if _, err := database.db.ExecContext(ctx, `DROP TRIGGER reject_rotation`); err != nil {
		t.Fatal(err)
	}
	appendDecision("refused", 6)
	head, err := database.VerifyAuditChain(ctx)
	if err != nil || head.ID != 6 {
		t.Fatalf("decision chain: %+v, %v", head, err)
	}
}

func TestPinRotationAuditRecheckRejectsTamperedEvidence(t *testing.T) {
	for _, tamper := range []string{"decision", "head"} {
		t.Run(tamper, func(t *testing.T) {
			now := time.Now().UTC()
			database := testStore(t, &now)
			ctx := context.Background()
			spec := rotationAuditSpec(t, rotationAuditFields(), "authorized")
			if _, err := database.AppendAuditAction(ctx, spec); err != nil {
				t.Fatal(err)
			}
			if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "operator", Action: "other", ResourceType: "system"}); err != nil {
				t.Fatal(err)
			}
			id := 1
			if tamper == "head" {
				id = 2
			}
			if _, err := database.db.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.ExecContext(ctx, `UPDATE audit_actions SET actor = 'tampered' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := database.AppendAuditAction(ctx, spec); err == nil {
				t.Fatal("reused a decision with corrupt audit evidence")
			}
		})
	}
}
