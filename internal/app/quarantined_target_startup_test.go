package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/testdb"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type quarantinedStartupFixture struct {
	db         *store.Store
	root, path string
	repo       model.Repository
	run, other model.Run
	evidence   []string
	resources  []model.ExecutionTargetResource
}

func newQuarantinedStartupFixture(t *testing.T) quarantinedStartupFixture {
	t.Helper()
	ctx := t.Context()
	f := quarantinedStartupFixture{root: t.TempDir()}
	f.path = filepath.Join(f.root, "state.db")
	db, err := storetest.Open(ctx, f.path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/acme"})
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	runs := make([]model.Run, 2)
	for i, ref := range []string{"feature/quarantined", "feature/adoptable"} {
		if _, err = storetest.EnqueueRun(db, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat([]string{"a", "b"}[i], 40), Actor: "fixture", Trigger: "branch"}); err != nil {
			t.Fatal(err)
		}
		run, err := db.ClaimNextRun(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.SetRunWorkflowName(ctx, run.ID, []string{"old-target-workflow", "other-workflow"}[i]); err != nil {
			t.Fatal(err)
		}
		runs[i], err = db.MarkRunSubmitted(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.run, f.other = runs[0], runs[1]
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	if _, err = seed.Exec(testdb.PostNativeTargetPlacementDowngrade); err != nil {
		t.Fatal(err)
	}
	binding := model.ExecutionTargetBinding{RunID: f.run.ID, Workflow: f.run.WorkflowName, Target: "remote", RepositoryID: f.repo.ID, Namespace: "remote", Snapshot: json.RawMessage(`{"Config":{"ExecutionPlacement":{"node_name":"old-node","namespace":"remote"}}}`)}
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(`INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json,admission_json) VALUES(?,?,?,'{}')`, f.run.ID, f.run.WorkflowName, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(`INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,'Pod','remote','source','original-uid','{"metadata":{"annotations":{}}}',1)`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = store.OpenAdminClient(ctx, f.path, store.Options{})
	if err != nil {
		t.Fatal("migration aborted", err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	if status, err := f.db.ExecutionTargetRecoveryStatus(ctx); err != nil || status == nil || status.QuarantinedBindings != 1 {
		t.Fatal(status, err)
	}
	f.resources, err = f.db.ExecutionTargetResources(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.evidence = f.custodyEvidence(t)
	return f
}

func (f quarantinedStartupFixture) custodyEvidence(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var binding, admission, digest, quarantine, cancelled, cleanup string
	if err = db.QueryRow(`SELECT binding_json,admission_json,custody_digest,quarantine_reason,cancelled,cleanup_receipt FROM execution_target_bindings WHERE run_id=?`, f.run.ID).Scan(&binding, &admission, &digest, &quarantine, &cancelled, &cleanup); err != nil {
		t.Fatal(err)
	}
	return []string{binding, admission, digest, quarantine, cancelled, cleanup}
}

type targetRecoveryControl struct {
	configuredController
	expected                    string
	readErr                     error
	entered                     chan string
	adoptions, creates, cancels atomic.Int32
}

func (c *targetRecoveryControl) Adopt(_ context.Context, r argojob.AdoptionRequest) (argojob.Adoption, bool, error) {
	c.adoptions.Add(1)
	if r.RunID != c.expected {
		return argojob.Adoption{}, false, errors.New("quarantined run reached a controller")
	}
	return argojob.Adoption{UID: "other-workflow-uid", Identity: "other-identity"}, c.readErr == nil, c.readErr
}
func (c *targetRecoveryControl) WaitResumed(ctx context.Context, _ string, run string, _ io.Writer, _ []runprogress.Event, _ int64, _ argojob.Adoption) (argojob.Completion, error) {
	c.entered <- run
	<-ctx.Done()
	return argojob.Completion{}, ctx.Err()
}
func (c *targetRecoveryControl) Create(context.Context, argojob.Request) (string, error) {
	c.creates.Add(1)
	return "", errors.New("unexpected submission during recovery")
}
func (c *targetRecoveryControl) Cancel(context.Context, string, string) error {
	c.cancels.Add(1)
	return errors.New("unexpected object cleanup")
}

func (f quarantinedStartupFixture) scheduler(t *testing.T, c *targetRecoveryControl) (*service.Scheduler, *ArgoJobs) {
	t.Helper()
	local, err := NewArgoJobs(c, argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"}, f.db, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := WithExecutionTargets(local, f.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(f.root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []model.Run{f.run, f.other} {
		file, err := logs.Create(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: f.db, Git: collectGit{}, Logs: logs, Jobs: jobs, Auditor: f.db, WorkspaceRoot: filepath.Join(f.root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, jobs
}

// The reviewer's migration -> routed Adopt -> Scheduler.Run reproducer now
// also requires a second Workflow to be adopted and observed after readiness.
func TestReviewerQuarantinedTargetCannotStopStartup(t *testing.T) {
	f := newQuarantinedStartupFixture(t)
	c := &targetRecoveryControl{expected: f.other.ID, entered: make(chan string, 1)}
	scheduler, jobs := f.scheduler(t, c)
	if _, err := jobs.Adopt(t.Context(), f.run); !errors.Is(err, service.ErrAdoptionRefused) || !errors.Is(err, store.ErrExecutionTargetQuarantined) {
		t.Fatal("known quarantine is not a per-run refusal", err)
	}
	marker := filepath.Join(f.root, "work", f.run.ID, "retained.txt")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("retained recovery evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var result error
	go func() { result = scheduler.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if result != nil {
				t.Error(result)
			}
		case <-time.After(testtimeout.Start(5 * time.Second)):
			t.Error("scheduler did not join")
		}
	})
	select {
	case run := <-c.entered:
		if run != f.other.ID || !scheduler.Recovered() {
			t.Fatal("unrelated recovery did not complete", run)
		}
	case <-done:
		t.Fatal("quarantined target stopped startup", result)
	case <-time.After(testtimeout.Start(5 * time.Second)):
		t.Fatal("other Workflow was not observed")
	}
	failed, err := f.db.Run(t.Context(), f.run.ID)
	if err != nil || failed.Status != model.RunFailed || failed.Phase != "infrastructure" || !strings.Contains(failed.Error, "quarantined") || !strings.Contains(failed.Error, f.run.WorkflowName) {
		t.Fatal("quarantine not visible on its run", failed, err)
	}
	other, err := f.db.Run(t.Context(), f.other.ID)
	if err != nil || other.Status != model.RunRunning {
		t.Fatal("other recovery affected", other, err)
	}
	assertQuarantineIsInert(t, f, jobs)
	if c.adoptions.Load() != 1 || c.creates.Load() != 0 || c.cancels.Load() != 0 {
		t.Fatal("quarantine authorized effects", c.adoptions.Load(), c.creates.Load(), c.cancels.Load())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained recovery evidence" {
		t.Fatal("recovery workspace removed", string(data), err)
	}
}

func assertQuarantineIsInert(t *testing.T, f quarantinedStartupFixture, jobs *ArgoJobs) {
	t.Helper()
	ctx := t.Context()
	if _, err := jobs.Adopt(ctx, f.run); !errors.Is(err, service.ErrAdoptionRefused) {
		t.Fatal("quarantine reused after failure", err)
	}
	if err := jobs.CreateWorkflow(ctx, service.WorkflowRequest{Run: f.run, Repository: f.repo, WorkflowName: f.run.WorkflowName}); !errors.Is(err, store.ErrExecutionTargetQuarantined) {
		t.Fatal("quarantine authorized a submission", err)
	}
	if err := jobs.Delete(ctx, f.run.WorkflowName, f.run.ID); !errors.Is(err, store.ErrExecutionTargetQuarantined) {
		t.Fatal("quarantine authorized deletion", err)
	}
	if err := jobs.ReconcileTargetCleanup(ctx); err != nil {
		t.Fatal("quarantine entered automatic cleanup", err)
	}
	if _, found, err := f.db.NextExecutionTargetCleanup(ctx, ""); err != nil || found {
		t.Fatal("quarantine selected for cleanup", found, err)
	}
	if err := f.db.CompleteExecutionTargetCleanup(ctx, f.run.ID); !errors.Is(err, store.ErrExecutionTargetQuarantined) {
		t.Fatal("quarantine cleanup completed", err)
	}
	if got := f.custodyEvidence(t); !reflect.DeepEqual(got, f.evidence) {
		t.Fatal("quarantined custody rewritten", got, f.evidence)
	}
	resources, err := f.db.ExecutionTargetResources(ctx, f.run.ID)
	if err != nil || !reflect.DeepEqual(resources, f.resources) {
		t.Fatal("original object/UID evidence changed", resources, err)
	}
	if eligible, err := f.db.RunWorkspaceCleanupEligible(ctx, f.run.ID); err != nil || eligible {
		t.Fatal("quarantine workspace eligible", eligible, err)
	}
	if ids, err := f.db.RunWorkspaceCleanupCandidates(ctx, "", 100); err != nil || slices.Contains(ids, f.run.ID) {
		t.Fatal("quarantine workspace selected", ids, err)
	}
	retained, err := f.db.EvidenceRetentionRuns(ctx, 0)
	if err != nil || !retained[f.run.ID].Protected {
		t.Fatal("quarantine evidence unprotected", retained, err)
	}
	if _, err := f.db.PurgeRunEvidence(ctx, "operator", f.run.ID, "both", "retention check", 0, func() (int64, error) { t.Error("quarantine evidence removed"); return 0, nil }); !errors.Is(err, store.ErrEvidenceProtected) {
		t.Fatal("quarantine evidence purge accepted", err)
	}
	if _, err := f.db.VerifyAuditChain(ctx); err != nil {
		t.Fatal("audit history damaged", err)
	}
}

func TestTargetRecoveryReadFailuresRemainFatal(t *testing.T) {
	for _, kind := range []string{"database", "transport", "other-invalid-state"} {
		t.Run(kind, func(t *testing.T) {
			f := newQuarantinedStartupFixture(t)
			failure := io.ErrUnexpectedEOF
			if kind == "other-invalid-state" {
				failure = store.ErrInvalidState
			}
			c := &targetRecoveryControl{expected: f.other.ID, readErr: failure, entered: make(chan string, 1)}
			scheduler, jobs := f.scheduler(t, c)
			if kind == "database" {
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := jobs.Adopt(t.Context(), f.other); err == nil || errors.Is(err, service.ErrAdoptionRefused) || errors.Is(err, store.ErrExecutionTargetQuarantined) {
				t.Fatal("read failure was classified as quarantine", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(5*time.Second))
			defer cancel()
			err := scheduler.Run(ctx)
			if err == nil || scheduler.Recovered() || errors.Is(err, service.ErrAdoptionRefused) {
				t.Fatal("genuine read failure did not stop startup", err)
			}
			if kind != "database" {
				if !errors.Is(err, failure) {
					t.Fatal("read failure cause lost", err)
				}
				run, err := f.db.Run(t.Context(), f.other.ID)
				if err != nil || run.Status != model.RunRunning {
					t.Fatal("read failure became a run verdict", run, err)
				}
			}
		})
	}
}
