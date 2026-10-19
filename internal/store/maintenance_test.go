package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func maintenanceFixture(t *testing.T) (*Store, *time.Time, context.Context) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	return testStore(t, &now), &now, context.Background()
}

func bookNow(t *testing.T, s *Store, ctx context.Context) model.MaintenanceWindow {
	t.Helper()
	w, err := s.BookMaintenance(ctx, model.MaintenanceSpec{BookedBy: "admin@host", Reason: "upgrade", DrainTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestMigrationV25CreatesWindowTableAndContractRow(t *testing.T) {
	s, _, ctx := maintenanceFixture(t)
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 25 {
		t.Fatalf("migration version = %d, %v", version, err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT id FROM maintenance_windows`); err != nil {
		t.Fatal(err)
	}
}

func TestBookRefusesSecondOpenWindow(t *testing.T) {
	s, _, ctx := maintenanceFixture(t)
	first := bookNow(t, s, ctx)
	if first.State != model.MaintenanceDraining {
		t.Fatalf("state = %s", first.State)
	}
	_, err := s.BookMaintenance(ctx, model.MaintenanceSpec{BookedBy: "other", DrainTimeout: time.Minute})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second book = %v", err)
	}
	var actions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_actions WHERE action='maintenance.book'`).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("book actions = %d, %v", actions, err)
	}
}

func TestClaimNextRunSkipsQueuedWhileDrainingAndActive(t *testing.T) {
	s, _, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "one", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "two", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	if _, err := s.ClaimNextRun(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("draining claim = %v", err)
	}
	activated, p, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, w.CreatedAt)
	if err != nil || !activated || p.HeldRuns != 2 {
		t.Fatalf("activation = %v, %+v, %v", activated, p, err)
	}
	if _, err := s.ClaimNextRun(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("active claim = %v", err)
	}
	if err := s.EndMaintenance(ctx, w.ID, "admin@host", "ended"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		run, err := s.ClaimNextRun(ctx)
		if err != nil || run.ID != id {
			t.Fatalf("claim = %s, %v, want %s", run.ID, err, id)
		}
	}
}

func TestBookedFutureWindowDoesNotHold(t *testing.T) {
	s, now, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	_, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "one", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.BookMaintenance(ctx, model.MaintenanceSpec{BookedBy: "admin", NotBefore: now.Add(time.Minute), DrainTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if w.State != model.MaintenanceBooked {
		t.Fatalf("state = %s", w.State)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginMaintenanceDrain(ctx, w.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("draining claim = %v", err)
	}
}

func TestActivateOnlyWhenQuietAndPendingCancellation(t *testing.T) {
	s, now, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	r, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "one", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	if ok, p, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, *now); err != nil || ok || p.RunningRuns != 1 {
		t.Fatalf("running activation = %v, %+v, %v", ok, p, err)
	}
	if _, err := s.FinishRun(ctx, r.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO run_cancellations(run_id,job_name,reason,created_at) VALUES(?,?,'owner_restart',?)`, r.ID, "job-one", unixNano(*now)); err != nil {
		t.Fatal(err)
	}
	if ok, p, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, *now); err != nil || ok || p.PendingCancellations != 1 {
		t.Fatalf("pending activation = %v, %+v, %v", ok, p, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE run_cancellations SET completed_at=? WHERE run_id=?`, unixNano(*now), r.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, *now); err != nil || !ok {
		t.Fatalf("quiet activation = %v, %v", ok, err)
	}
}

func TestAbortOnDrainTimeoutWithoutForce(t *testing.T) {
	s, now, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "one", strings.Repeat("a", 40))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	if ok, err := s.AbortMaintenanceIfExpired(ctx, w.ID, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("abort = %v, %v", ok, err)
	}
	if _, err := s.CurrentMaintenance(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current = %v", err)
	}
	var reason string
	if err := s.db.QueryRowContext(ctx, `SELECT end_reason FROM maintenance_windows WHERE id=?`, w.ID).Scan(&reason); err != nil || reason != "drain_timeout" {
		t.Fatalf("reason = %q, %v", reason, err)
	}
}

func TestForcedWindowNeverAbortsAndInterruptsBranch(t *testing.T) {
	s, now, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	r, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "one", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, r.ID, "job-one"); err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	forced, err := s.ForceMaintenance(ctx, w.ID, "admin")
	if err != nil || len(forced) != 1 || forced[0].Reason != "maintenance" {
		t.Fatalf("force = %+v, %v", forced, err)
	}
	if ok, err := s.AbortMaintenanceIfExpired(ctx, w.ID, now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("forced abort = %v, %v", ok, err)
	}
	if ok, p, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, *now); err != nil || ok || p.PendingCancellations != 1 {
		t.Fatalf("forced activation = %v, %+v, %v", ok, p, err)
	}
	var reason string
	if err := s.db.QueryRowContext(ctx, `SELECT reason FROM run_cancellations WHERE run_id=?`, r.ID).Scan(&reason); err != nil || reason != "owner_restart" {
		t.Fatalf("cancellation = %q, %v", reason, err)
	}
}

func TestTerminalWindowImmutable(t *testing.T) {
	s, _, ctx := maintenanceFixture(t)
	w := bookNow(t, s, ctx)
	if _, _, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, w.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.EndMaintenance(ctx, w.ID, "admin", "ended"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE maintenance_windows SET reason='changed' WHERE id=?`, w.ID); err == nil {
		t.Fatal("terminal row was updated")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM maintenance_windows WHERE id=?`, w.ID); err == nil {
		t.Fatal("terminal row was deleted")
	}
}

func TestEnqueueReceiveEventReportsHold(t *testing.T) {
	for _, future := range []bool{false, true} {
		t.Run(map[bool]string{false: "draining", true: "booked"}[future], func(t *testing.T) {
			s, now, ctx := maintenanceFixture(t)
			repo := createRepo(t, s)
			at := *now
			if future {
				at = at.Add(time.Minute)
			}
			w, err := s.BookMaintenance(ctx, model.MaintenanceSpec{BookedBy: "admin", Reason: "upgrade", NotBefore: at, DrainTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			result, err := s.EnqueueReceiveEvent(ctx, model.ReceiveEventSpec{ID: "receive-1", Actor: "pusher", RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/hold", ObjectSHA: sha, CommitSHA: sha, Outcome: "queued"},
				model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/hold", SHA: sha, Actor: "pusher", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if future && result.Maintenance != nil {
				t.Fatalf("booked window held run: %+v", result.Maintenance)
			}
			if !future && (result.Maintenance == nil || result.Maintenance.WindowID != w.ID) {
				t.Fatalf("missing hold: %+v", result.Maintenance)
			}
		})
	}
}

func TestForceInterruptsOnlyUnprotectedRuns(t *testing.T) {
	s, _, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	var ids []string
	for i, kind := range []string{"branch", "release", "grouped", "publication"} {
		spec := testRunSpec(repo.ID, kind, strings.Repeat(string(rune('a'+i)), 40))
		if kind == "release" {
			spec.RefKind, spec.Ref, spec.Release, spec.Trigger = model.RefTag, "v1", true, "tag"
		}
		r, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRunJobName(ctx, r.ID, "job-"+kind); err != nil {
			t.Fatal(err)
		}
		if kind == "grouped" {
			if err := s.SetRunConcurrencyGroup(ctx, r.ID, "terraform"); err != nil {
				t.Fatal(err)
			}
		}
		if kind == "publication" {
			if _, err := s.BeginPublication(ctx, model.PublicationSpec{
				RepoID: repo.ID, RunID: r.ID, RefKind: model.RefBranch, Ref: r.Ref,
				PreviousSHA: strings.Repeat("e", 40), ResultSHA: r.SHA, Actor: r.Actor,
			}); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, r.ID)
	}
	const previous = "dddddddddddddddddddddddddddddddddddddddd"
	const resultSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/promotion", SourceSHA: strings.Repeat("f", 40), TargetRef: "main", Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err = s.PlanPromotion(ctx, promotion.ID, previous, resultSHA)
	if err != nil {
		t.Fatal(err)
	}
	admitted, _, err := s.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/" + resultSHA[:12], SHA: resultSHA, Actor: "admin", Trigger: "promotion", TestedSHA: resultSHA, BaseSHA: previous}, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, admitted.ID, "job-promotion"); err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	progress, err := s.MaintenanceProgress(ctx)
	if err != nil || progress.ProtectedRuns != 3 {
		t.Fatalf("protected progress = %+v, %v", progress, err)
	}
	forced, err := s.ForceMaintenance(ctx, w.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(forced) != 2 || forced[0].ID != ids[0] || forced[1].ID != admitted.ID || forced[0].Reason != "maintenance" || forced[1].Reason != "maintenance" {
		t.Fatalf("forced = %+v", forced)
	}
	for _, id := range ids[1:] {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=?`, id).Scan(&status); err != nil || status != "running" {
			t.Fatalf("protected status = %q, %v", status, err)
		}
	}
	var obligations int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM run_cancellations WHERE run_id=? AND reason='owner_restart'`, ids[0]).Scan(&obligations); err != nil || obligations != 1 {
		t.Fatalf("obligations = %d, %v", obligations, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM run_cancellations WHERE run_id=? AND reason='owner_restart'`, admitted.ID).Scan(&obligations); err != nil || obligations != 1 {
		t.Fatalf("promotion obligations = %d, %v", obligations, err)
	}
	var promotionStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM promotions WHERE id=?`, promotion.ID).Scan(&promotionStatus); err != nil || promotionStatus != "interrupted" {
		t.Fatalf("promotion = %q, %v", promotionStatus, err)
	}
	audit, err := s.LatestAuditActionByType(ctx, "maintenance.force", "maintenance_window")
	if err != nil || !strings.Contains(audit.Details, ids[0]) || !strings.Contains(audit.Details, admitted.ID) || !strings.Contains(audit.Details, ids[1]) || !strings.Contains(audit.Details, ids[2]) || !strings.Contains(audit.Details, ids[3]) {
		t.Fatalf("force audit = %+v, %v", audit, err)
	}
}

func TestForcedBranchRunIsRetryEligible(t *testing.T) {
	s, now, ctx := maintenanceFixture(t)
	repo := createRepo(t, s)
	r, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/retry", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	w := bookNow(t, s, ctx)
	if _, err := s.ForceMaintenance(ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.ActivateMaintenanceIfQuiet(ctx, w.ID, *now); err != nil || !ok {
		t.Fatalf("activate = %v, %v", ok, err)
	}
	if err := s.EndMaintenance(ctx, w.ID, "admin", "ended"); err != nil {
		t.Fatal(err)
	}
	retried, err := s.RetryRun(ctx, r.ID, "admin")
	if err != nil || retried.Status != model.RunQueued || retried.ID == r.ID {
		t.Fatalf("retry = %+v, %v", retried, err)
	}
}
