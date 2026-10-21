package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

type retentionFixture struct {
	retention *Retention
	api       *API
	repo      model.Repository
	root      string
	now       time.Time
	count     int
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	f := &retentionFixture{root: t.TempDir(), now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	db, err := store.Open(context.Background(), filepath.Join(f.root, "oberth.sqlite"), store.Options{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := db.CreateUpstream(context.Background(), model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme"})
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = db.CreateRepository(context.Background(), model.RepositorySpec{Name: "retention", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(f.root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := artifacts.Open(filepath.Join(f.root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	f.retention = &Retention{Database: db, Logs: logs, Artifacts: kept, DataRoot: f.root, Now: func() time.Time { return f.now }}
	f.api, err = NewAPI(APIConfig{Runs: db, History: db, Repositories: db, Logs: logs, Artifacts: kept, Retention: f.retention})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *retentionFixture) run(t *testing.T, ref, trigger string, terminal bool) model.Run {
	t.Helper()
	ctx := context.Background()
	f.count++
	enqueued, err := f.retention.Database.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: fmt.Sprintf("%040x", f.count), Actor: "operator", Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	run := enqueued.Run
	if terminal {
		if _, err := f.retention.Database.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.retention.Database.PutStepResult(ctx, model.StepResult{RunID: run.ID, Burn: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: "M"}); err != nil {
			t.Fatal(err)
		}
		run, err = f.retention.Database.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: run.SHA})
		if err != nil {
			t.Fatal(err)
		}
	}
	file, err := f.retention.Logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[test/unit] retained output\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "artifacts", run.ID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	return run
}

func (f *retentionFixture) retained(t *testing.T, id string, want bool) {
	t.Helper()
	for _, path := range []string{filepath.Join(f.root, "logs", id+".log"), filepath.Join(f.root, "artifacts", id, "report.txt")} {
		_, err := os.Stat(path)
		if (err == nil) != want || (err != nil && !errors.Is(err, os.ErrNotExist)) {
			t.Fatalf("evidence %s: err=%v, want retained=%v", path, err, want)
		}
	}
}

func TestRunPurgeAuditsRemovalAndPreservesHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	run := f.run(t, "feature/purge", "branch", true)
	actor := api.Actor{Identity: "owner", Admin: true}
	arguments, _ := json.Marshal(map[string]string{"id": run.ID[:12], "what": "both", "reason": "remove retained output"})
	value, err := f.api.CallTool(ctx, actor, "run_purge", arguments)
	if err != nil {
		t.Fatal(err)
	}
	purge := value.(model.EvidencePurge)
	if purge.State != "complete" || purge.Bytes <= int64(len("report")) || purge.Actor != "owner" || purge.Reason != "remove retained output" {
		t.Fatalf("purge=%#v", purge)
	}
	f.retained(t, run.ID, false)
	detail, err := f.api.Run(ctx, actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := detail.(RunDetailResponse)
	if history.Run.ID != run.ID || len(history.Steps) != 1 || len(history.Purges) != 1 || history.Purges[0].State != "complete" {
		t.Fatalf("history lost after purge: %#v", history)
	}
	for name, args := range map[string]string{
		"run_logs":     fmt.Sprintf(`{"id":%q,"burn":"test","step":"unit"}`, run.ID),
		"artifacts":    fmt.Sprintf(`{"id":%q}`, run.ID),
		"artifact_get": fmt.Sprintf(`{"id":%q,"name":"report.txt"}`, run.ID),
	} {
		_, err := f.api.CallTool(ctx, actor, name, json.RawMessage(args))
		if !errors.Is(err, ErrEvidencePurged) || !strings.Contains(err.Error(), "purged by owner on ") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.api.RunLogLive(ctx, actor, run.ID, 0); !errors.Is(err, ErrEvidencePurged) {
		t.Fatalf("live purged log=%v", err)
	}
	if _, err := f.retention.Database.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
	// A retry remains idempotent and records its own operation.
	again, err := f.retention.Purge(ctx, actor, run.ID, "both", "repeat removal")
	if err != nil || again.Bytes != 0 || again.ID == purge.ID {
		t.Fatalf("repeat=%#v %v", again, err)
	}
}

func TestRunPurgeRefusesNonAdminScopedAndAuditFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	run := f.run(t, "feature/authorization", "branch", true)
	args := json.RawMessage(fmt.Sprintf(`{"id":%q,"what":"both","reason":"cleanup"}`, run.ID))
	for _, actor := range []api.Actor{{Identity: "ordinary"}, {Identity: "scoped-admin", Admin: true, Scope: []string{"github"}}} {
		if _, err := f.api.CallTool(ctx, actor, "run_purge", args); !errors.Is(err, ErrForbidden) {
			t.Fatalf("actor=%#v err=%v", actor, err)
		}
	}
	f.retention.MutationGate = func(context.Context) error { return errors.New("unverified audit") }
	if _, err := f.retention.Purge(ctx, api.Actor{Identity: "admin", Admin: true}, run.ID, "both", "cleanup"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("gate=%v", err)
	}
	f.retained(t, run.ID, true)
	markers, err := f.retention.Database.RunEvidencePurges(ctx, run.ID)
	if err != nil || len(markers) != 0 {
		t.Fatalf("refused purge wrote markers: %#v %v", markers, err)
	}
}

func TestRetentionAgeAndBudgetKeepProtectedAndNewestEvidence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"age", "budget"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newRetentionFixture(t)
			old := f.run(t, "feature/history", "branch", true)
			protected := f.run(t, "feature/pending", "branch", true)
			_, err := f.retention.Database.AppendPromotion(ctx, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: protected.Ref, SourceSHA: protected.SHA, TargetRef: "main", Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			release := f.run(t, "v1", "release", true)
			newest := f.run(t, "feature/history", "branch", true)
			active := f.run(t, "feature/active", "branch", false)
			f.now = f.now.Add(90 * 24 * time.Hour)
			f.retention.KeepNewest = 1
			if kind == "age" {
				f.retention.LogPolicy.Age = 24 * time.Hour
				f.retention.ArtifactPolicy.Age = 24 * time.Hour
			} else {
				f.retention.LogPolicy.BudgetBytes = 1
				f.retention.ArtifactPolicy.BudgetBytes = 1
			}
			if err := f.retention.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			f.retained(t, old.ID, false)
			for _, run := range []model.Run{protected, release, newest, active} {
				f.retained(t, run.ID, true)
			}
			status, err := f.retention.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			observed := status.(RetentionStatus)
			if observed.LogsBytes == 0 || observed.ArtifactsBytes == 0 || observed.LastSweep.IsZero() || observed.LastError != "" || observed.FreeBytes <= 0 {
				t.Fatalf("status=%#v", observed)
			}
		})
	}
}

func TestRetentionDefaultsKeepLogsAndTimerSweepsWithoutCollection(t *testing.T) {
	t.Parallel()
	f := newRetentionFixture(t)
	run := f.run(t, "feature/timer", "branch", true)
	f.now = f.now.Add(90 * 24 * time.Hour)
	if err := f.retention.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.retained(t, run.ID, true)
	f.retention.LogPolicy.Age = time.Hour
	f.retention.ArtifactPolicy.Age = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// End after the completed sweep observes that artifact evidence is gone,
	// avoiding a timing assumption about filesystem or audit-commit latency.
	f.retention.Now = func() time.Time {
		if _, err := os.Stat(filepath.Join(f.root, "artifacts", run.ID, "report.txt")); errors.Is(err, os.ErrNotExist) {
			cancel()
		}
		return f.now
	}
	f.retention.Run(ctx, time.Hour, log.New(io.Discard, "", 0))
	f.retained(t, run.ID, false)
}

func TestRunPurgeNeverOverridesEvidenceProtection(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"active", "release", "pending promotion", "open issue", "active grant"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newRetentionFixture(t)
			trigger := "branch"
			if kind == "release" {
				trigger = "release"
			}
			run := f.run(t, "feature/protected", trigger, kind != "active")
			switch kind {
			case "pending promotion":
				if _, err := f.retention.Database.AppendPromotion(ctx, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: run.Ref, SourceSHA: run.SHA, TargetRef: "main", Actor: "owner"}); err != nil {
					t.Fatal(err)
				}
			case "open issue":
				if _, err := f.retention.Database.UpsertCIIssue(ctx, "owner", f.repo.ID, run.Ref, "open investigation", "evidence needed"); err != nil {
					t.Fatal(err)
				}
			case "active grant":
				if _, err := f.retention.Database.AppendPromotionGrant(ctx, model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: run.SHA, TargetRef: "main", Issuer: "owner", IssuerFingerprint: "SHA256:owner", ExpiresAt: f.now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.retention.Purge(ctx, api.Actor{Identity: "owner", Admin: true}, run.ID, "both", "cleanup"); !errors.Is(err, store.ErrEvidenceProtected) {
				t.Fatalf("%s purge: %v", kind, err)
			}
			f.retained(t, run.ID, true)
		})
	}
}

func TestRunPurgeSelectsOnlyRequestedEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	run := f.run(t, "feature/select", "branch", true)
	actor := api.Actor{Identity: "owner", Admin: true}
	if _, err := f.retention.Purge(ctx, actor, run.ID, "artifacts", "remove report"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "logs", run.ID+".log")); err != nil {
		t.Fatal(err)
	}
	if err := f.retention.EvidenceError(ctx, run.ID, "logs"); err != nil {
		t.Fatalf("artifact purge suppressed logs: %v", err)
	}
	if err := f.retention.EvidenceError(ctx, run.ID, "artifacts"); !errors.Is(err, ErrEvidencePurged) {
		t.Fatalf("artifact marker=%v", err)
	}
	for _, request := range []struct{ what, reason string }{{"all", "cleanup"}, {"logs", ""}, {"logs", "two\nlines"}, {"logs", strings.Repeat("x", 513)}} {
		if _, err := f.retention.Purge(ctx, actor, run.ID, request.what, request.reason); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("request=%#v err=%v", request, err)
		}
	}
	if _, err := f.retention.Purge(ctx, actor, run.ID, "logs", "remove transcript"); err != nil {
		t.Fatal(err)
	}
	f.retained(t, run.ID, false)
}

func TestPurgeWaitsForCancelledSchedulerWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newControlFixture(t)
	jobs := &completingJobs{started: make(chan struct{}), release: make(chan struct{}), result: JobResult{Status: model.RunInterrupted, Phase: "interrupted"}}
	fixture.scheduler.jobs = jobs
	kept, err := artifacts.Open(filepath.Join(fixture.root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	retention := &Retention{Database: fixture.store, Logs: fixture.logs, Artifacts: kept, AcquirePurge: fixture.scheduler.AcquireEvidencePurge}
	run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/cancel-purge", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- fixture.scheduler.ProcessNext(ctx) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(jobs.release) }) }
	defer release()
	select {
	case <-jobs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler did not start")
	}
	cancelled, err := fixture.store.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "owner", Reason: "stop run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelled.Cancellations) != 1 {
		t.Fatalf("cancellation=%#v", cancelled)
	}
	if err := fixture.store.CompleteRunCancellation(ctx, run.ID, cancelled.Cancellations[0].JobName); err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "owner", Admin: true}
	if _, err := retention.Purge(ctx, actor, run.ID, "both", "remove cancelled evidence"); !errors.Is(err, store.ErrEvidenceProtected) {
		t.Fatalf("purged while scheduler writer alive: %v", err)
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler did not finish")
	}
	if _, err := retention.Purge(ctx, actor, run.ID, "both", "remove cancelled evidence"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "logs", run.ID+".log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log survived purge: %v", err)
	}
}

func TestTerminalClaimCannotRecreatePurgedEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newControlFixture(t)
	run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/late-dispatch", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "owner", Reason: "cancel before dispatch"}); err != nil {
		t.Fatal(err)
	}
	// The claim's goroutine begins only after the terminal state is durable.
	if err := fixture.scheduler.executeAndCleanup(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "logs", run.ID+".log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late dispatch recreated a log: %v", err)
	}
}

func TestProtectedPurgeRetryResolvesCrashedIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	run := f.run(t, "feature/crashed-purge", "branch", true)
	// Interrupt the attempt after its pending commit, before its result commit.
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	pending, err := f.retention.Database.PurgeRunEvidence(interrupted, "owner", run.ID, "both", "cleanup", 0, func() (int64, error) {
		cancel()
		return 0, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.retention.EvidenceError(ctx, run.ID, "logs"); !errors.Is(err, ErrEvidencePurged) {
		t.Fatalf("unresolved intent not fenced: %v", err)
	}
	if _, err := f.retention.Database.UpsertCIIssue(ctx, "owner", f.repo.ID, run.Ref, "keep evidence", "investigation"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Purge(ctx, api.Actor{Identity: "owner", Admin: true}, run.ID, "both", "retry cleanup"); !errors.Is(err, store.ErrEvidenceProtected) {
		t.Fatalf("protected retry: %v", err)
	}
	markers, err := f.retention.Database.RunEvidencePurges(ctx, run.ID)
	if err != nil || len(markers) != 1 || markers[0].ID != pending.ID || markers[0].State != "refused" {
		t.Fatalf("stale intent was not resolved: %#v %v", markers, err)
	}
	for _, what := range []string{"logs", "artifacts"} {
		if err := f.retention.EvidenceError(ctx, run.ID, what); err != nil {
			t.Fatalf("refused retry still fences %s: %v", what, err)
		}
	}
	f.retained(t, run.ID, true)
	if _, err := f.retention.Database.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionReclaimsOrphansOnlyForEnabledEvidencePolicies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	orphan := strings.Repeat("f", 32)
	// Mount-point housekeeping must not become an orphan purge candidate.
	lostFound := filepath.Join(f.root, "artifacts", "lost+found")
	if err := os.MkdirAll(lostFound, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := f.retention.Logs.Create(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("orphan output\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "artifacts", orphan)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "report.txt"), []byte("orphan report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.retention.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	f.retained(t, orphan, true)
	f.retention.LogPolicy.Age = time.Hour
	if err := f.retention.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "logs", orphan+".log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan log survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "report.txt")); err != nil {
		t.Fatalf("disabled artifact policy removed evidence: %v", err)
	}
	f.retention.ArtifactPolicy.BudgetBytes = 1024 // Orphans are reclaimed even below budget.
	if err := f.retention.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	f.retained(t, orphan, false)
	if _, err := os.Stat(lostFound); err != nil {
		t.Fatalf("sweep touched mount-point housekeeping: %v", err)
	}
	if markers, err := f.retention.Database.RunEvidencePurges(ctx, "lost+found"); err != nil || len(markers) != 0 {
		t.Fatalf("invalid directory entered the purge audit: %#v %v", markers, err)
	}
	markers, err := f.retention.Database.RunEvidencePurges(ctx, orphan)
	if err != nil || len(markers) != 2 {
		t.Fatalf("orphan audit=%#v %v", markers, err)
	}
	for _, marker := range markers {
		if marker.State != "complete" || marker.Bytes <= 0 || marker.Reason != "orphaned evidence" {
			t.Fatalf("orphan audit=%#v", marker)
		}
	}
	if _, err := f.retention.Database.Run(ctx, orphan); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan purge created history: %v", err)
	}
	if _, err := f.retention.Database.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := f.retention.Status(ctx)
	if err != nil || status.(RetentionStatus).LogsBytes != 0 || status.(RetentionStatus).ArtifactsBytes != 0 {
		t.Fatalf("orphan totals=%#v %v", status, err)
	}
}

func TestRetentionSweepPrefiltersProtectedRuns(t *testing.T) {
	t.Parallel()
	f := newRetentionFixture(t)
	run := f.run(t, "v1", "release", true)
	f.now = f.now.Add(24 * time.Hour)
	f.retention.LogPolicy.Age = time.Hour
	f.retention.ArtifactPolicy.BudgetBytes = 1
	f.retention.MutationGate = func(context.Context) error {
		t.Error("protected candidate entered the purge mutation path")
		return errors.New("unexpected mutation")
	}
	if err := f.retention.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.retained(t, run.ID, true)
}

func TestRetentionStatusUsesLastSweepTotals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRetentionFixture(t)
	f.run(t, "feature/cached-status", "branch", true)
	if err := f.retention.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := f.retention.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.(RetentionStatus).LogsBytes <= 0 || before.(RetentionStatus).ArtifactsBytes <= 0 {
		t.Fatalf("empty measurements: %#v", before)
	}
	// Make both trees unreadable as directories. system_status must remain a
	// cached read, and a failed sweep must preserve the last measured totals.
	for _, name := range []string{"logs", "artifacts"} {
		path := filepath.Join(f.root, name)
		if err := os.Rename(path, path+"-saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.retention.Status(ctx)
	if err != nil || after != before {
		t.Fatalf("status rescanned evidence: before=%#v after=%#v err=%v", before, after, err)
	}
	if err := f.retention.Sweep(ctx); err == nil {
		t.Fatal("expected failed scan")
	}
	failed, err := f.retention.Status(ctx)
	if err != nil || failed.(RetentionStatus).LastError == "" || failed.(RetentionStatus).LogsBytes != before.(RetentionStatus).LogsBytes {
		t.Fatalf("failed sweep lost cached totals: %#v %v", failed, err)
	}
}
