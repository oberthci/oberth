package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdminPriorityLoweringPersistsAndCapsAging(t *testing.T) {
	for _, priority := range []model.RunPriority{"normal", "low"} {
		t.Run(string(priority), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "promotion/main/one", strings.Repeat("a", 40))
			spec.Trigger = "promotion"
			first, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			normal, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/next", strings.Repeat("b", 40)))
			if err != nil {
				t.Fatal(err)
			}
			lowered, err := s.BumpRunPriority(t.Context(), "admin", first.ID, priority, "let ordinary work proceed")
			if err != nil {
				t.Fatal(err)
			}
			if lowered.Priority != priority.Canonical() || !strings.Contains(lowered.PriorityReason, "let ordinary work proceed") {
				t.Fatalf("lowered=%+v", lowered)
			}
			now = now.Add(2 * time.Hour)
			got, err := s.Run(t.Context(), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if p, why := got.EffectivePriority(now); p != priority.Canonical() || !strings.Contains(why, "let ordinary work proceed") {
				t.Fatalf("aging erased admin priority: %s %s", p, why)
			}
			next, err := s.NextQueuedRun(t.Context())
			if err != nil || next.ID != normal.ID {
				t.Fatalf("lowered run still blocks head: %+v %v", next, err)
			}
			reopened, err := openAdminClientForTest(t.Context(), s.livePath, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err = reopened.Run(t.Context(), first.ID)
			if err != nil || got.Priority != priority.Canonical() {
				t.Fatalf("restart lost priority: %+v %v", got, err)
			}
			var actor, details string
			if err = s.db.QueryRow(`SELECT actor,details FROM audit_actions WHERE action='run.priority' AND resource_id=?`, first.ID).Scan(&actor, &details); err != nil {
				t.Fatal(err)
			}
			var audit map[string]any
			if err = json.Unmarshal([]byte(details), &audit); err != nil {
				t.Fatal(err)
			}
			if actor != "admin" || audit["previous_priority"] != "promotion" || audit["priority"] != string(priority.Canonical()) || audit["reason"] != "let ordinary work proceed" {
				t.Fatalf("audit=%s by %s", details, actor)
			}
		})
	}
}

func TestAdminPriorityMigrationPreservesOverridesAndAudit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	queued, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/upgrade", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BumpRunPriority(t.Context(), "admin", queued.ID, model.PriorityHigh, "preserved reason"); err != nil {
		t.Fatal(err)
	}
	before, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	migrationID := "20261104030321-admin-run-priority-3253bc64cd6ece41cf5675e06457b29d"
	// Restore the previous table shape without this migration. Later timestamp
	// IDs model independently applied migrations; compact their local receipt
	// ordinals so the fixture remains a valid, contiguous ledger.
	var ordinal int
	if err = s.db.QueryRow(`SELECT version FROM schema_migrations WHERE id=?`, migrationID).Scan(&ordinal); err != nil {
		t.Fatal(err)
	}
	legacy := strings.ReplaceAll(queuePrioritySchema, "run_priority_overrides", "run_priority_overrides_legacy")
	_, err = s.db.Exec(legacy+`; INSERT INTO run_priority_overrides_legacy SELECT * FROM run_priority_overrides; DROP TABLE run_priority_overrides; ALTER TABLE run_priority_overrides_legacy RENAME TO run_priority_overrides; DELETE FROM schema_migrations WHERE id=?`, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE schema_migrations SET version=-version WHERE version>?; UPDATE schema_migrations SET version=-version-1 WHERE version<0`, ordinal); err != nil {
		t.Fatal(err)
	}

	var actor, reason string
	var at int64
	if err = s.db.QueryRow(`SELECT actor,reason,updated_at FROM run_priority_overrides WHERE run_id=?`, queued.ID).Scan(&actor, &reason, &at); err != nil {
		t.Fatal(err)
	}
	path := s.livePath
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openAdminClientForTest(t.Context(), path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var gotActor, gotReason string
	var gotAt int64
	if err = reopened.db.QueryRow(`SELECT actor,reason,updated_at FROM run_priority_overrides WHERE run_id=?`, queued.ID).Scan(&gotActor, &gotReason, &gotAt); err != nil {
		t.Fatal(err)
	}
	if actor != gotActor || reason != gotReason || at != gotAt {
		t.Fatal("migration rewrote prior admin receipt")
	}
	after, err := reopened.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed audit history")
	}
	if _, err = reopened.BumpRunPriority(t.Context(), "admin", queued.ID, model.PriorityLow, "new lower priority"); err != nil {
		t.Fatal(err)
	}
}

func TestAdminPriorityKeepsShippedMigration39Body(t *testing.T) {
	// Captured from the prerequisite build before changing runtime classification.
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[38].sql))); got != "4a6340cef25e08bb50d64bb9395efef18b5923ecd95eb832232ab6500215881b" {
		t.Fatalf("shipped migration 39 changed: %s", got)
	}
}
