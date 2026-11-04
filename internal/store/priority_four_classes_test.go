package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestFourClassesDurableQueueOrderAndAuthority(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	admin, ordinary := true, false
	enqueue := func(name string, priority model.RunPriority, authority *bool, promotion bool) model.Run {
		t.Helper()
		spec := testRunSpec(repo.ID, name, strings.Repeat("a", 40))
		spec.PriorityRequest = model.PriorityRequest{Priority: priority, PriorityReason: "explicit request", UnscopedAdmin: authority}
		if promotion {
			spec.Trigger = "promotion"
		}
		run, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return run.Run
	}
	standard1 := enqueue("standard1", "", &ordinary, false)
	low1 := enqueue("low1", "low", &ordinary, false)
	promotion1 := enqueue("promotion1", "", &ordinary, true)
	high1 := enqueue("high1", "high", &admin, false)
	standard2 := enqueue("standard2", "standard", &ordinary, false)
	low2 := enqueue("low2", "low", &ordinary, false)
	promotion2 := enqueue("promotion2", "", &ordinary, true)
	high2 := enqueue("high2", "highest", &admin, false) // legacy authenticated client
	refused := enqueue("refused", "high", &ordinary, false)
	if refused.Priority != "standard" || refused.PriorityRefusal == "" {
		t.Errorf("unauthorized high=%+v", refused)
	}
	for _, run := range []model.Run{promotion1, promotion2} {
		if run.Priority != "promotion" {
			t.Errorf("automatic promotion=%s, must not consume high", run.Priority)
		}
	}
	want := []model.Run{high1, high2, promotion1, promotion2, standard1, standard2, refused, low1, low2}
	listed, err := s.ListRecentRuns(t.Context(), model.RunListFilter{})
	if err != nil || len(listed) != len(want) {
		t.Fatalf("list count=%d err=%v", len(listed), err)
	}
	for i, expected := range want {
		if listed[i].ID != expected.ID || listed[i].QueuePosition != int64(i+1) {
			t.Errorf("list position %d=%s want %s", i, listed[i].Ref, expected.Ref)
		}
		got, err := s.ClaimNextRun(t.Context())
		if err != nil || got.ID != expected.ID {
			t.Fatalf("claim %d=%s want %s: %v", i, got.Ref, expected.Ref, err)
		}
	}
}

func TestFourClassesLoweringSurvivesReopenAndAudit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	for i, target := range []model.RunPriority{"promotion", "standard", "low"} {
		spec := testRunSpec(repo.ID, fmt.Sprint("urgent-", i), strings.Repeat("a", 40))
		admin := true
		spec.PriorityRequest = model.PriorityRequest{Priority: "high", PriorityReason: "urgent", UnscopedAdmin: &admin}
		spec.Trigger = "promotion"
		run, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		lowered, err := s.BumpRunPriority(t.Context(), "admin", run.ID, target, "deliberately defer")
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Hour)
		reopened, err := OpenAdminClient(t.Context(), s.livePath, Options{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		got, err := reopened.Run(t.Context(), run.ID)
		_ = reopened.Close()
		if err != nil {
			t.Fatal(err)
		}
		p, _ := got.EffectivePriority(now)
		if lowered.Priority != target || p != target {
			t.Fatalf("lowering %s lost: %+v", target, got)
		}
		var details string
		if err := s.db.QueryRow(`SELECT details FROM audit_actions WHERE action='run.priority' AND resource_id=?`, run.ID).Scan(&details); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(details, `"previous_priority":"high"`) || !strings.Contains(details, `"priority":"`+string(target)+`"`) || !strings.Contains(details, "deliberately defer") {
			t.Fatalf("audit=%s", details)
		}
	}
}

func TestFourClassesLegacyGrantProjectionAndCappedAuthority(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	if _, err := s.db.Exec(`INSERT INTO priority_grants(identity,max_class,expires,granted_by) VALUES('legacy','highest',?,'admin')`, unixNano(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	grants, err := s.ListPriorityGrants(t.Context())
	if err != nil || len(grants) != 1 || grants[0].MaxClass != "high" {
		t.Fatalf("legacy grant not canonical: %+v %v", grants, err)
	}
	if _, err := s.SetPriorityGrant(t.Context(), "admin", model.PriorityGrant{Identity: "worker", MaxClass: "promotion", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ordinary := false
	for _, class := range []model.RunPriority{"promotion", "high", "highest"} {
		spec := testRunSpec(repo.ID, string(class), strings.Repeat("a", 40))
		spec.Actor = "worker"
		spec.PriorityRequest = model.PriorityRequest{Priority: class, PriorityReason: "delegated work", UnscopedAdmin: &ordinary}
		got, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if class == "promotion" {
			if got.Priority != "promotion" || got.PriorityRefusal != "" {
				t.Fatalf("permitted middle class=%+v", got.Run)
			}
		} else if got.Priority != "standard" || !strings.Contains(got.PriorityRefusal, "exceeds") {
			t.Fatalf("grant silently acquired urgent high: %+v", got.Run)
		}
	}
}

func TestFourClassesUpgradePreservesLegacyPriorityEvidence(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	makeRun := func(ref, trigger string) model.Run {
		t.Helper()
		spec := testRunSpec(repo.ID, ref, strings.Repeat("a", 40))
		spec.Trigger = trigger
		r, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	ordinary := makeRun("legacy-normal", "push")
	urgent := makeRun("legacy-explicit", "push")
	automatic := makeRun("legacy-promotion", "promotion")
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO run_priority_overrides VALUES(?,'normal','archived deferral','admin',?)`, []any{ordinary.ID, unixNano(now)}},
		{`INSERT INTO priority_grants VALUES('legacy','highest',?,'admin')`, []any{unixNano(now.Add(time.Hour))}},
		{`INSERT INTO run_priority_requests VALUES(?,'highest','highest','archived urgent request','legacy','')`, []any{urgent.ID}},
		{`UPDATE run_priority_requests SET accepted='highest' WHERE run_id=?`, []any{automatic.ID}},
	} {
		if _, err := s.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	// Reconstruct the exact old CHECK vocabulary, retaining all raw receipts.
	// Remove later empty resource tables and receipts together so the legacy
	// ledger has no gap when the complete migration catalog is replayed.
	ddl := `DROP TABLE resource_attempts; DROP TABLE resource_attempt_inventories; DROP TABLE pod_resource_dispositions;
 CREATE TABLE run_priority_overrides_old (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,priority TEXT NOT NULL CHECK(priority IN ('low','normal','high','highest')),reason TEXT NOT NULL,actor TEXT NOT NULL,updated_at INTEGER NOT NULL) WITHOUT ROWID;
 INSERT INTO run_priority_overrides_old SELECT * FROM run_priority_overrides; DROP TABLE run_priority_overrides; ALTER TABLE run_priority_overrides_old RENAME TO run_priority_overrides;
 CREATE TABLE priority_grants_old (identity TEXT PRIMARY KEY,max_class TEXT NOT NULL CHECK(max_class IN ('high','highest')),expires INTEGER NOT NULL,granted_by TEXT NOT NULL) WITHOUT ROWID;
 INSERT INTO priority_grants_old SELECT * FROM priority_grants; DROP TABLE priority_grants; ALTER TABLE priority_grants_old RENAME TO priority_grants;
 CREATE TABLE run_priority_requests_old (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,requested TEXT NOT NULL,accepted TEXT NOT NULL CHECK(accepted IN ('normal','high','highest')),reason TEXT NOT NULL,actor TEXT NOT NULL,refusal TEXT NOT NULL) WITHOUT ROWID;
 INSERT INTO run_priority_requests_old SELECT * FROM run_priority_requests; DROP TABLE run_priority_requests; ALTER TABLE run_priority_requests_old RENAME TO run_priority_requests;
 DELETE FROM schema_migrations WHERE id IN ('20261104190000-priority-four-classes','20261104203255-pod-resource-dispositions-de9abcf59e57071e796e5aae3d614e3c','20261104220233-resource-attempt-inventory-53d49c721cd86410289f1448a4686d70');`
	if _, err := s.db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "admin", Action: "fixture.legacy", ResourceType: "queue", ResourceID: "priority", Details: `{"priority":"highest","reason":"historical receipt"}`}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []string {
		t.Helper()
		var all []string
		for _, table := range []string{"run_priority_overrides", "priority_grants", "run_priority_requests"} {
			rows, err := s.db.Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
			if err != nil {
				t.Fatal(err)
			}
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				values := make([]any, len(cols))
				ptr := make([]any, len(cols))
				for i := range values {
					ptr[i] = &values[i]
				}
				if err := rows.Scan(ptr...); err != nil {
					t.Fatal(err)
				}
				all = append(all, fmt.Sprint(values))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
		}
		return all
	}
	before := snapshot()
	auditBefore, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("migration rewrote raw priority evidence: %v != %v", got, before)
	}
	auditAfter, err := s.VerifyAuditChain(t.Context())
	if err != nil || !reflect.DeepEqual(auditBefore, auditAfter) {
		t.Fatalf("audit chain changed: %+v %+v %v", auditBefore, auditAfter, err)
	}
	for id, want := range map[string]model.RunPriority{ordinary.ID: "standard", urgent.ID: "high", automatic.ID: "promotion"} {
		got, err := s.Run(t.Context(), id)
		if err != nil || got.Priority != want {
			t.Fatalf("legacy projection %s=%s want %s: %v", id, got.Priority, want, err)
		}
	}
	reopened, err := OpenAdminClient(t.Context(), s.livePath, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.BumpRunPriority(t.Context(), "admin", ordinary.ID, "promotion", "new middle class after upgrade"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.SetPriorityGrant(t.Context(), "admin", model.PriorityGrant{Identity: "middle", MaxClass: "promotion", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}
