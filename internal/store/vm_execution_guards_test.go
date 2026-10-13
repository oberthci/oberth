package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func requireVMJournalGuard(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("wanted database guard %q, got %v", message, err)
	}
}

func vmJournalSnapshot(t *testing.T, s *Store, runID string) []any {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM vm_executions WHERE run_id=?`, runID)
	mustPilot(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	mustPilot(t, err)
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	if !rows.Next() {
		t.Fatalf("missing journal row: %v", rows.Err())
	}
	mustPilot(t, rows.Scan(targets...))
	return values
}

func TestVMJournalDatabaseIdentityGuards(t *testing.T) {
	for _, kind := range []string{"vm", "kvm-preflight"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			var runID, otherKind string
			if kind == "vm" {
				intent := vmIntent(t, s, createRepo(t, s), "guard")
				_, err := s.ReserveVMExecution(context.Background(), intent)
				mustPilot(t, err)
				runID, otherKind = intent.Spec.RunID, "kvm-preflight"
			} else {
				intent := kvmIntent(t, s)
				_, err := s.ReserveKVMPreflight(context.Background(), intent)
				mustPilot(t, err)
				runID, otherKind = intent.RunID, "vm"
			}
			before := vmJournalSnapshot(t, s, runID)
			// REPLACE can conflict through the capacity index as well as run_id.
			// A different valid run must not evict the active reservation.
			repo, err := s.RepositoryByName(context.Background(), "oberth")
			mustPilot(t, err)
			other := vmIntent(t, s, repo, "other")
			for _, cleaned := range []any{0, nil} {
				// REPLACE substitutes NOT NULL DEFAULT 0 after BEFORE INSERT
				// when cleaned is explicitly NULL, so both forms need protection.
				_, err = s.db.Exec(`INSERT OR REPLACE INTO vm_executions(run_id,execution_kind,intent_json,spec_identity,created_at,updated_at,cleaned)
                SELECT ?,execution_kind,intent_json,spec_identity,created_at,updated_at,? FROM vm_executions WHERE run_id=?`, other.Spec.RunID, cleaned, runID)
				requireVMJournalGuard(t, err, "VM executions are append-only")
				if !reflect.DeepEqual(before, vmJournalSnapshot(t, s, runID)) {
					t.Fatal("cross-run replacement evicted reservation")
				}
			}
			for _, attack := range []struct{ name, sql string }{
				{"kind", `UPDATE vm_executions SET execution_kind='` + otherKind + `' WHERE run_id=?`},
				{"kind-null", `UPDATE OR REPLACE vm_executions SET execution_kind=NULL WHERE run_id=?`},
				{"intent", `UPDATE vm_executions SET intent_json='{}' WHERE run_id=?`},
				{"spec", `UPDATE vm_executions SET spec_identity='replacement' WHERE run_id=?`},
				{"run", `UPDATE vm_executions SET run_id=NULL WHERE run_id=?`},
				{"created", `UPDATE vm_executions SET created_at=created_at+1 WHERE run_id=?`},
			} {
				t.Run(attack.name, func(t *testing.T) {
					_, err := s.db.Exec(attack.sql, runID)
					requireVMJournalGuard(t, err, "VM execution evidence is immutable")
					if !reflect.DeepEqual(before, vmJournalSnapshot(t, s, runID)) {
						t.Fatal("rejected mutation changed journal")
					}
				})
			}
			_, err = s.db.Exec(`DELETE FROM vm_executions WHERE run_id=?`, runID)
			// Even deleting a reservation before submission must retain its history.
			requireVMJournalGuard(t, err, "VM executions are append-only")
			_, err = s.db.Exec(`UPDATE vm_executions SET intent_json=intent_json,spec_identity=spec_identity,
                execution_kind=execution_kind,updated_at=updated_at+1 WHERE run_id=?`, runID)
			mustPilot(t, err)
			_, err = s.db.Exec(`UPDATE vm_executions SET cleaning=1,cleaned=1 WHERE run_id=?`, runID)
			mustPilot(t, err)
			before = vmJournalSnapshot(t, s, runID)
			for _, cleaned := range []any{0, nil} {
				_, err = s.db.Exec(`UPDATE OR REPLACE vm_executions SET cleaned=? WHERE run_id=?`, cleaned, runID)
				requireVMJournalGuard(t, err, "VM execution evidence is immutable")
				if !reflect.DeepEqual(before, vmJournalSnapshot(t, s, runID)) {
					t.Fatal("cleanup state was reset")
				}
			}
		})
	}
}

func TestKVMJournalDatabaseEvidenceGuards(t *testing.T) {
	for _, outcome := range []string{"receipt_json", "refusal_json"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			_, err := s.ReserveKVMPreflight(context.Background(), intent)
			mustPilot(t, err)
			// Initial submission, UID binding and evidence recording remain possible.
			_, err = s.db.Exec(`UPDATE vm_executions SET submitted=1 WHERE run_id=?`, intent.RunID)
			mustPilot(t, err)
			_, err = s.db.Exec(`UPDATE vm_executions SET workflow_uid='workflow-uid' WHERE run_id=?`, intent.RunID)
			mustPilot(t, err)
			var evidence []byte
			if outcome == "receipt_json" {
				evidence, err = json.Marshal(kvmReceipt(intent))
			} else {
				evidence, err = json.Marshal(vmrunner.KVMRefusal{Code: "observation_refused", ObservedAt: now})
			}
			mustPilot(t, err)
			_, err = s.db.Exec(`UPDATE vm_executions SET `+outcome+`=? WHERE run_id=?`, string(evidence), intent.RunID)
			mustPilot(t, err)
			_, err = s.db.Exec(`UPDATE vm_executions SET workflow_uid=workflow_uid,`+outcome+`=`+outcome+`,cleaning=1,cleaned=1 WHERE run_id=?`, intent.RunID)
			mustPilot(t, err)
			before := vmJournalSnapshot(t, s, intent.RunID)
			for _, field := range []string{"workflow_uid", outcome} {
				for _, replacement := range []any{"", "replacement", nil} {
					_, err = s.db.Exec(`UPDATE OR REPLACE vm_executions SET `+field+`=? WHERE run_id=?`, replacement, intent.RunID)
					requireVMJournalGuard(t, err, "VM execution evidence is immutable")
					if !reflect.DeepEqual(before, vmJournalSnapshot(t, s, intent.RunID)) {
						t.Fatalf("%s changed accepted evidence", field)
					}
				}
			}
			_, err = s.db.Exec(`DELETE FROM vm_executions WHERE run_id=?`, intent.RunID)
			requireVMJournalGuard(t, err, "VM executions are append-only")
			// SQLite REPLACE must not erase a cleaned row and reset accepted evidence,
			// including when recursive triggers are disabled (SQLite's default).
			_, err = s.db.Exec(`INSERT OR REPLACE INTO vm_executions(run_id,execution_kind,intent_json,spec_identity,created_at,updated_at)
                SELECT run_id,execution_kind,intent_json,spec_identity,created_at,updated_at FROM vm_executions WHERE run_id=?`, intent.RunID)
			requireVMJournalGuard(t, err, "VM executions are append-only")
			if !reflect.DeepEqual(before, vmJournalSnapshot(t, s, intent.RunID)) {
				t.Fatal("replacement erased accepted evidence")
			}
			_, err = s.KVMPreflight(context.Background(), intent.RunID)
			mustPilot(t, err)
		})
	}
}
