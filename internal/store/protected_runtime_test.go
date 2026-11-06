package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/testdb"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func genericRuntimeFixture(t *testing.T, s *Store) protectedtests.GenericAdmission {
	t.Helper()
	i := kvmIntent(t, s)
	c := &argoworkflow.Capability{Namespace: i.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{protectedtests.GenericDeviceResource: 1}}
	_, err := s.SetWorkflowCapabilityGrant(t.Context(), i.RepoID, "operator", c)
	requireStoreNoError(t, err)
	a, err := protectedtests.NewGenericAdmission(i, c, 1234)
	requireStoreNoError(t, err)
	return a
}

func TestGenericPreflightAtomicAdmissionAndImmutableRuntime(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	a := genericRuntimeFixture(t, s)
	ctx := t.Context()
	_, err := s.db.Exec(`CREATE TRIGGER fail_generic_audit BEFORE INSERT ON audit_actions WHEN NEW.action='protected.runtime-admitted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if _, err = s.ReserveGenericPreflight(ctx, a); err == nil {
		t.Fatal("failed audit admitted runtime")
	}
	var count int
	requireStoreNoError(t, s.db.QueryRow(`SELECT (SELECT count(*) FROM vm_executions)+(SELECT count(*) FROM protected_runtime_admissions)+(SELECT count(*) FROM vm_capacity_slots WHERE run_id IS NOT NULL)`).Scan(&count))
	if count != 0 {
		t.Fatalf("partial admission/capacity survived audit failure: %d", count)
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_generic_audit`)
	requireStoreNoError(t, err)
	_, err = s.ReserveGenericPreflight(ctx, a)
	requireStoreNoError(t, err)
	_, err = s.ReserveGenericPreflight(ctx, a)
	requireStoreNoError(t, err)
	_, err = s.ReserveKVMPreflight(ctx, a.Intent)
	requireKVMStateError(t, err)
	changed := a
	changed.SupplementalGroup++
	_, err = s.ReserveGenericPreflight(ctx, changed)
	requireKVMStateError(t, err)
	stored, err := s.GenericAdmission(ctx, a.Intent.RunID)
	requireStoreNoError(t, err)
	if stored == nil || stored.Digest() != a.Digest() {
		t.Fatal("runtime admission changed")
	}
	for _, query := range []string{`UPDATE protected_runtime_admissions SET admission_json='{}'`, `DELETE FROM protected_runtime_admissions`, `INSERT OR REPLACE INTO protected_runtime_admissions SELECT * FROM protected_runtime_admissions`} {
		if _, err = s.db.Exec(query); err == nil {
			t.Fatalf("mutable runtime admission: %s", query)
		}
	}
	_, err = s.VerifyAuditState(ctx)
	requireStoreNoError(t, err)
}

func TestGenericPreflightRevocationBlocksAuthorityButAllowsCleanup(t *testing.T) {
	for _, point := range []string{"reserve", "submit", "receipt", "delivery"} {
		t.Run(point, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			a := genericRuntimeFixture(t, s)
			ctx := t.Context()
			id := a.Intent.RunID
			revoke := func() {
				_, err := s.SetWorkflowCapabilityGrant(ctx, a.Intent.RepoID, "operator", nil)
				requireStoreNoError(t, err)
			}
			if point == "reserve" {
				revoke()
				_, err := s.ReserveGenericPreflight(ctx, a)
				requireKVMStateError(t, err)
				if _, err = s.KVMPreflight(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatal("revoked grant reserved capacity")
				}
				return
			}
			_, err := s.ReserveGenericPreflight(ctx, a)
			requireStoreNoError(t, err)
			if point == "submit" {
				revoke()
				requireKVMStateError(t, s.SubmitKVMPreflight(ctx, id))
				requireStoreNoError(t, s.RecordKVMRefusal(ctx, id, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: now}))
				requireStoreNoError(t, s.CompleteKVMPreflightCleanup(ctx, id, ""))
				return
			}
			requireStoreNoError(t, s.SubmitKVMPreflight(ctx, id))
			requireStoreNoError(t, s.BindKVMPreflight(ctx, id, "workflow-uid"))
			if point == "receipt" {
				revoke()
				requireKVMStateError(t, s.RecordKVMReceipt(ctx, id, kvmReceipt(a.Intent)))
				requireStoreNoError(t, s.RecordKVMRefusal(ctx, id, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: now}))
				requireStoreNoError(t, s.CompleteKVMPreflightCleanup(ctx, id, "workflow-uid"))
				return
			}
			requireStoreNoError(t, s.RecordKVMReceipt(ctx, id, kvmReceipt(a.Intent)))
			requireStoreNoError(t, s.CompleteKVMPreflightCleanup(ctx, id, "workflow-uid"))
			requireStoreNoError(t, s.CheckGenericPreflightAuthority(ctx, id))
			revoke()
			requireKVMStateError(t, s.CheckGenericPreflightAuthority(ctx, id))
			_, err = s.SetRunWorkflowName(ctx, id, "credentialed-parent")
			requireKVMStateError(t, err)
			run, err := s.Run(ctx, id)
			requireStoreNoError(t, err)
			if run.WorkflowName != "" {
				t.Fatal("revocation delivered parent")
			}
		})
	}
}

// Reopen the actual file, then attack the projection with its immutable guards
// explicitly removed. Both audit gates must refuse before invoking mutations.
func TestGenericRuntimeDurableReopenAndAuditFences(t *testing.T) {
	for _, attack := range []string{"changed envelope", "changed digest", "changed timestamp", "missing metadata", "missing event"} {
		t.Run(attack, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			a := genericRuntimeFixture(t, s)
			ctx := t.Context()
			_, err := s.ReserveGenericPreflight(ctx, a)
			requireStoreNoError(t, err)
			requireStoreNoError(t, s.SubmitKVMPreflight(ctx, a.Intent.RunID))
			path := s.livePath
			requireStoreNoError(t, s.Close())
			s = openTestStore(t, path, &now)
			got, err := s.GenericAdmission(ctx, a.Intent.RunID)
			requireStoreNoError(t, err)
			if got == nil || got.Digest() != a.Digest() || got.SupplementalGroup != 1234 {
				t.Fatal("restart lost immutable runtime")
			}
			execution, err := s.KVMPreflight(ctx, a.Intent.RunID)
			requireStoreNoError(t, err)
			if !execution.Submitted || execution.WorkflowUID != "" || execution.Cleaned {
				t.Fatal("uncertain create obligation lost on reopen")
			}
			head, err := s.VerifyAuditState(ctx)
			requireStoreNoError(t, err)
			_, _, err = s.VerifyLocalAuditMutationState(ctx, model.AuditHead{}, acceptAuditMutationState)
			requireStoreNoError(t, err)
			_, _, err = s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, acceptAuditMutationState)
			requireStoreNoError(t, err)
			switch attack {
			case "changed envelope":
				_, err = s.db.Exec(`DROP TRIGGER protected_runtime_admission_immutable`)
				requireStoreNoError(t, err)
				changed := a
				changed.SupplementalGroup++
				body, e := json.Marshal(changed)
				requireStoreNoError(t, e)
				_, err = s.db.Exec(`UPDATE protected_runtime_admissions SET admission_json=?,admission_sha256=?`, string(body), changed.Digest())
			case "changed digest":
				_, err = s.db.Exec(`DROP TRIGGER protected_runtime_admission_immutable`)
				requireStoreNoError(t, err)
				_, err = s.db.Exec(`UPDATE protected_runtime_admissions SET intent_sha256=printf('%064d',0)`)
			case "changed timestamp":
				_, err = s.db.Exec(`DROP TRIGGER protected_runtime_admission_immutable`)
				requireStoreNoError(t, err)
				_, err = s.db.Exec(`UPDATE protected_runtime_admissions SET created_at=created_at+1`)
			case "missing metadata":
				_, err = s.db.Exec(`DROP TRIGGER protected_runtime_admission_no_delete`)
				requireStoreNoError(t, err)
				_, err = s.db.Exec(`DELETE FROM protected_runtime_admissions`)
			case "missing event":
				_, err = s.db.Exec(`DROP TRIGGER audit_actions_no_delete`)
				requireStoreNoError(t, err)
				_, err = s.db.Exec(`DELETE FROM audit_actions WHERE action='protected.runtime-admitted'`)
			}
			requireStoreNoError(t, err)
			if _, err = s.GenericAdmission(ctx, a.Intent.RunID); err == nil {
				t.Fatal("corrupt generic runtime treated as usable or legacy")
			}
			if _, err = s.VerifyAuditState(ctx); err == nil {
				t.Fatal("full audit accepted corrupt runtime")
			}
			if _, err = s.VerifyAuditStateSince(ctx, head.ID, head.SHA256); err == nil {
				t.Fatal("suffix audit accepted corrupt runtime")
			}
			called := false
			callback := func(model.AuditHead, model.AuditAnchor) error { called = true; return nil }
			if _, _, err = s.VerifyLocalAuditMutationState(ctx, head, callback); err == nil || called {
				t.Fatalf("local mutation escaped: called=%v err=%v", called, err)
			}
			if _, _, err = s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, callback); err == nil || called {
				t.Fatalf("external mutation escaped: called=%v err=%v", called, err)
			}
		})
	}
}

func TestGenericRuntimeMutationFenceRechecksProjection(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			ctx := t.Context()
			a := genericRuntimeFixture(t, s)
			_, err := s.ReserveGenericPreflight(ctx, a)
			requireStoreNoError(t, err)
			_, err = s.db.Exec(`DROP TRIGGER protected_runtime_admission_no_delete`)
			requireStoreNoError(t, err)
			called := false
			mutate := func(model.AuditHead, model.AuditAnchor) error {
				called = true
				_, e := s.db.Exec(`DELETE FROM protected_runtime_admissions`)
				return e
			}
			if local {
				_, _, err = s.VerifyLocalAuditMutationState(ctx, model.AuditHead{}, mutate)
			} else {
				_, _, err = s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, mutate)
			}
			if !called || err == nil {
				t.Fatalf("projection mutation escaped fence: called=%v err=%v", called, err)
			}
		})
	}
}

func TestGenericRuntimeMigrationPreservesLegacyProtectedJournal(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := t.Context()
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	var before, digest string
	requireStoreNoError(t, s.db.QueryRow(`SELECT intent_json,spec_identity FROM vm_executions WHERE run_id=?`, intent.RunID).Scan(&before, &digest))
	// Reconstruct exactly the predecessor's schema/ledger; published SQL bodies
	// are unchanged. Then exercise the real pending-migration production opener.
	_, err = s.db.Exec(testdb.PostProtectedRuntimeDowngrade)
	requireStoreNoError(t, err)
	path := s.livePath
	requireStoreNoError(t, s.Close())
	s = openTestStore(t, path, &now)
	var after, afterDigest string
	requireStoreNoError(t, s.db.QueryRow(`SELECT intent_json,spec_identity FROM vm_executions WHERE run_id=?`, intent.RunID).Scan(&after, &afterDigest))
	if before != after || digest != afterDigest {
		t.Fatal("migration rewrote legacy host intent")
	}
	admission, err := s.GenericAdmission(ctx, intent.RunID)
	requireStoreNoError(t, err)
	if admission != nil {
		t.Fatal("migration silently upgraded legacy runtime")
	}
	execution, err := s.KVMPreflight(ctx, intent.RunID)
	requireStoreNoError(t, err)
	if !execution.Submitted || execution.Cleaned || execution.WorkflowUID != "" {
		t.Fatal("migration lost uncertain-create obligation")
	}
	_, err = s.VerifyAuditState(ctx)
	requireStoreNoError(t, err)
}
