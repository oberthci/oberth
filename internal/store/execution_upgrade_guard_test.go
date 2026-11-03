package store

import (
	"path/filepath"
	"testing"
)

func TestExecutionUpgradePreservesInFlightWorkForRecovery(t *testing.T) {
	for _, scenario := range []string{"execution", "capacity", "running", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "upgrade.sqlite")
			s, err := OpenAdminClient(ctx, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			intent := kvmIntent(t, s)
			if scenario == "execution" {
				if _, err = s.ReserveKVMPreflight(ctx, intent); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec(`DELETE FROM vm_capacity_slots`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "capacity" {
				if _, err = s.db.Exec(`INSERT INTO vm_capacity_slots(slot,run_id) VALUES(1,?)`, intent.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "running" {
				if _, err = s.db.Exec(`UPDATE runs SET status='interrupted',finished_at=updated_at WHERE id=?`, intent.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cleanup" {
				if _, err = s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at) VALUES(?,'legacy-workflow','','owner_restart',1)`, intent.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.db.Exec(resourceCeilingDowngradeSQL + `DELETE FROM schema_migrations WHERE version=47`); err != nil {
				t.Fatal(err)
			}
			candidate, err := OpenAdminClient(ctx, path, Options{})
			if candidate != nil {
				_ = candidate.Close()
			}
			if err != nil {
				t.Fatalf("busy upgrade must reach owner recovery for %s: %v", scenario, err)
			}
			var applied int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=47`).Scan(&applied); err != nil || applied != 1 {
				t.Fatalf("compatible upgrade not recorded: %d %v", applied, err)
			}
			// Migration must not mark an unobserved external object as cleaned
			// or finish a run/cancellation on the strength of schema work alone.
			var executions, slots, running, cleanup int
			err = s.db.QueryRow(`SELECT
                (SELECT COUNT(*) FROM vm_executions WHERE cleaned=0),
                (SELECT COUNT(*) FROM vm_capacity_slots),
                (SELECT COUNT(*) FROM runs WHERE status='running'),
                (SELECT COUNT(*) FROM run_cancellations WHERE completed_at IS NULL)`).Scan(&executions, &slots, &running, &cleanup)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{"execution": executions, "capacity": slots, "running": running, "cleanup": cleanup}
			if got[scenario] != 1 {
				t.Fatalf("upgrade lost %s obligation: %+v", scenario, got)
			}

		})
	}
}

func TestExecutionUpgradeAllowsDrainedGrantsAndLaterRecovery(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	s, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	if _, err = s.SetKubeVirtGrant(ctx, repo.ID, "admin", true, KubeVirtBudget{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(resourceCeilingDowngradeSQL + `DELETE FROM schema_migrations WHERE version=47`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatalf("drained grant blocked upgrade: %v", err)
	}
	intent := vmIntent(t, s, repo, "after-upgrade")
	if _, err = s.ReserveVMExecution(ctx, intent); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatalf("already-upgraded legacy recovery blocked: %v", err)
	}
	defer func() { _ = s.Close() }()
	pending, err := s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 1 || pending[0].Intent.Spec.RunID != intent.Spec.RunID {
		t.Fatalf("legacy recovery obligation lost: %+v %v", pending, err)
	}
}
