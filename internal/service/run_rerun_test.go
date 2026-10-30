package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const (
	rerunBranch    = "feature/flaky"
	rerunSHA       = "abababababababababababababababababababab"
	rerunNewerSHA  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	rerunTag       = "v1.2.3"
	rerunTagObject = "dededededededededededededededededededede"
	rerunTagCommit = "efefefefefefefefefefefefefefefefefefefef"
)

// tagPushGit fails the first failTags tag pushes before the remote moves, the
// way a forge 5xx or SSH blip leaves a tag unpublished.
type tagPushGit struct {
	*controlGit
	mu       sync.Mutex
	failTags int
	attempts int
}

func (git *tagPushGit) SyncTag(ctx context.Context, repo, tag, sha string) error {
	git.mu.Lock()
	git.attempts++
	fail := git.failTags > 0
	if fail {
		git.failTags--
	}
	git.mu.Unlock()
	if fail {
		return errors.New("forge returned 502")
	}
	return git.controlGit.SyncTag(ctx, repo, tag, sha)
}

func (git *tagPushGit) tagAttempts() int {
	git.mu.Lock()
	defer git.mu.Unlock()
	return git.attempts
}

type rerunHarness struct {
	fixture   *controlFixture
	git       *tagPushGit
	refs      stubRefResolver
	scheduler *Scheduler
	service   *API
}

func newRerunHarness(t *testing.T, results ...JobResult) *rerunHarness {
	t.Helper()
	fixture := newControlFixture(t, results...)
	harness := &rerunHarness{fixture: fixture, git: &tagPushGit{controlGit: fixture.git},
		refs: stubRefResolver{branches: map[string]map[string]string{"codeberg/acme/oberth": {rerunBranch: rerunSHA}}}}
	var err error
	harness.scheduler, err = NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: harness.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "rerun-work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.service, err = NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, RunRetries: fixture.store, RunReruns: fixture.store,
		Repositories: fixture.store, Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: harness.scheduler, Git: harness.git, Refs: harness.refs, Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: testTimeout(50 * time.Millisecond),
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "rerun-promotion-work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

func (harness *rerunHarness) call(actor api.Actor, tool string, arguments map[string]any) (any, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	return harness.service.CallTool(context.Background(), actor, tool, encoded)
}

func (harness *rerunHarness) rerun(t *testing.T, actor api.Actor, arguments map[string]any) api.RunRerunResponse {
	t.Helper()
	value, err := harness.call(actor, "run_rerun", arguments)
	if err != nil {
		t.Fatalf("run_rerun %v: %v", arguments, err)
	}
	response, ok := value.(api.RunRerunResponse)
	if !ok {
		t.Fatalf("run_rerun response = %#v", value)
	}
	return response
}

func (harness *rerunHarness) process(t *testing.T) {
	t.Helper()
	if err := harness.scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (harness *rerunHarness) run(t *testing.T, id string) model.Run {
	t.Helper()
	run, err := harness.fixture.store.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

var (
	rerunUplink = api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	rerunAdmin  = api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	flakyResult = JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit",
		Steps: []model.StepResult{stepResult(model.StepFailed)}}
	greenResult  = JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
	infraFailure = JobResult{Status: model.RunFailed, Phase: "job", Error: "pod evicted"}
)

// A flaky test failure is rerun at the same SHA without a push; the rerun
// passes, links to the original, and SHA-keyed status and wait follow it.
func TestRunRerunAfterTestFailurePassesAndStatusReportsNewestRun(t *testing.T) {
	harness := newRerunHarness(t, flakyResult, greenResult)
	ctx := context.Background()
	first, err := harness.scheduler.EnqueueCI(ctx, CIRequest{EventID: "flaky-push", Repository: harness.fixture.repo,
		Branch: rerunBranch, SHA: rerunSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	harness.process(t)
	failed := harness.run(t, first.ID)
	if failed.Status != model.RunFailed || failed.FailedStep == "" {
		t.Fatalf("first run = %#v, want a test failure", failed)
	}
	if _, err := harness.call(rerunUplink, "run_retry", map[string]any{"id": failed.ID}); err == nil {
		t.Fatal("run_retry accepted a test failure; run_rerun would be redundant")
	}

	response := harness.rerun(t, rerunUplink, map[string]any{"id": failed.ID[:8], "reason": "flaky integration test"})
	if response.Mode != "rerun" || response.RerunOf != failed.ID || response.RunID == "" || response.SHA != rerunSHA || response.Ref != rerunBranch {
		t.Fatalf("run_rerun = %#v", response)
	}
	rerun := harness.run(t, response.RunID)
	if rerun.Status != model.RunQueued || rerun.SHA != failed.SHA || rerun.Ref != failed.Ref || rerun.Trigger != failed.Trigger || rerun.Actor != rerunUplink.Identity {
		t.Fatalf("queued rerun = %#v", rerun)
	}
	if _, err := harness.call(rerunUplink, "run_rerun", map[string]any{"id": failed.ID}); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("rerun while the branch has an active run error = %v, want ErrInvalidState", err)
	}

	harness.process(t)
	if passed := harness.run(t, rerun.ID); passed.Status != model.RunPassed {
		t.Fatalf("rerun = %#v, want passed", passed)
	}
	value, err := harness.call(rerunUplink, "status", map[string]any{"ref": rerunSHA})
	if err != nil {
		t.Fatal(err)
	}
	if status := value.(StatusResponse); status.Run.ID != rerun.ID || status.Run.Status != model.RunPassed {
		t.Fatalf("status of %s = run %s %s, want the passed rerun %s", rerunSHA, status.Run.ID, status.Run.Status, rerun.ID)
	}
	value, err = harness.call(rerunUplink, "wait", map[string]any{"sha": rerunSHA})
	if err != nil {
		t.Fatal(err)
	}
	if wait := value.(WaitResponse); wait.Run.ID != rerun.ID || wait.StillRunning {
		t.Fatalf("wait on %s = %#v, want the rerun", rerunSHA, wait.Run)
	}
	value, err = harness.call(rerunUplink, "run_get", map[string]any{"id": rerun.ID})
	if err != nil {
		t.Fatal(err)
	}
	if detail := value.(RunDetailResponse); detail.RerunOf != failed.ID {
		t.Fatalf("run_get rerun_of = %q, want %s", detail.RerunOf, failed.ID)
	}
	if action, err := harness.fixture.store.LatestAuditActionByType(ctx, "run.rerun", "run"); err != nil ||
		action.ResourceID != rerun.ID || !strings.Contains(action.Details, failed.ID) || !strings.Contains(action.Details, "flaky integration test") {
		t.Fatalf("run.rerun audit = %#v, %v", action, err)
	}
	// The original is no longer the newest run of its branch, and can still
	// be rerun while its SHA is the branch head (the passed rerun is terminal).
	older := harness.rerun(t, rerunUplink, map[string]any{"id": failed.ID})
	if older.RunID == "" || older.RerunOf != failed.ID {
		t.Fatalf("rerun of a non-newest run = %#v", older)
	}
}

// Rerunning a commit the branch has moved past would force-sync the older
// commit over the newer push, so it is refused.
func TestRunRerunRefusesBranchThatMoved(t *testing.T) {
	harness := newRerunHarness(t, flakyResult)
	ctx := context.Background()
	first, err := harness.scheduler.EnqueueCI(ctx, CIRequest{EventID: "old-push", Repository: harness.fixture.repo,
		Branch: rerunBranch, SHA: rerunSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	harness.process(t)
	harness.refs.branches["codeberg/acme/oberth"][rerunBranch] = rerunNewerSHA
	_, err = harness.call(rerunUplink, "run_rerun", map[string]any{"id": first.ID})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), rerunNewerSHA) {
		t.Fatalf("rerun after the branch moved error = %v, want ErrInvalidInput naming %s", err, rerunNewerSHA)
	}
	if runs, _ := harness.fixture.store.ListRecentRuns(ctx, model.RunListFilter{RepoID: harness.fixture.repo.ID, Limit: 10}); len(runs) != 1 {
		t.Fatalf("runs after a refused rerun = %d, want 1", len(runs))
	}
}

func admitRerunTag(t *testing.T, harness *rerunHarness) model.Run {
	t.Helper()
	queued, err := harness.scheduler.AdmitRelease(context.Background(), ReleaseRequest{EventID: "tag-push", Repository: harness.fixture.repo,
		Tag: rerunTag, ObjectSHA: rerunTagObject, CommitSHA: rerunTagCommit, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	harness.process(t)
	return harness.run(t, queued.ID)
}

// A tag run that failed before publishing is rerun only by an admin with
// force; the rerun publishes the tag once, and the tag can never be rerun or
// published again.
func TestTagRerunNeedsAdminForceAndNeverPublishesTwice(t *testing.T) {
	harness := newRerunHarness(t, infraFailure, greenResult)
	failed := admitRerunTag(t, harness)
	if failed.Status != model.RunFailed || !failed.Release || len(harness.git.syncedTags) != 0 {
		t.Fatalf("tag run = %#v, synced %v; want an unpublished failure", failed, harness.git.syncedTags)
	}
	if _, err := harness.call(rerunUplink, "run_rerun", map[string]any{"id": failed.ID, "force": true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin tag rerun error = %v, want ErrForbidden", err)
	}
	if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": failed.ID}); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "force") {
		t.Fatalf("admin tag rerun without force error = %v, want ErrInvalidInput asking for force", err)
	}
	response := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true, "reason": "evicted"})
	if response.Mode != "rerun" || response.Warning == "" {
		t.Fatalf("forced tag rerun = %#v, want a rerun with the release warning", response)
	}
	rerun := harness.run(t, response.RunID)
	if !rerun.Release || !rerun.Credentialed || rerun.RefKind != model.RefTag || rerun.SHA != rerunTagObject || rerun.TestedSHA != rerunTagCommit {
		t.Fatalf("tag rerun = %#v, want the same admitted tag object and commit", rerun)
	}
	harness.process(t)
	if passed := harness.run(t, rerun.ID); passed.Status != model.RunPassed {
		t.Fatalf("tag rerun = %#v, want passed", passed)
	}
	if len(harness.git.syncedTags) != 1 || !strings.HasSuffix(harness.git.syncedTags[0], rerunTag+":"+rerunTagObject) {
		t.Fatalf("tag publications = %v, want exactly one of the admitted object", harness.git.syncedTags)
	}
	for _, id := range []string{failed.ID, rerun.ID} {
		if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": id, "force": true}); err == nil {
			t.Fatalf("rerun of %s after the tag was published was accepted", id)
		}
	}
	if harness.git.tagAttempts() != 1 {
		t.Fatalf("tag push attempts = %d, want 1", harness.git.tagAttempts())
	}
}

// A tag run whose release steps all passed and whose tag push alone failed is
// not re-executed: the rerun re-pushes the recorded tag object, as
// publish_retry does, and nothing is published a second time.
func TestTagRerunAfterPublicationFailureRetriesPublicationOnly(t *testing.T) {
	harness := newRerunHarness(t, greenResult)
	harness.git.failTags = 1
	failed := admitRerunTag(t, harness)
	if failed.Status != model.RunFailed || failed.Phase != "publishing" {
		t.Fatalf("tag run = %#v, want failed/publishing", failed)
	}
	response := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true})
	if response.Mode != "publish_retry" || response.RunID != "" || response.Status != string(model.PublicationDelivered) {
		t.Fatalf("rerun of a publishing failure = %#v, want a delivered publication retry and no new run", response)
	}
	if passed := harness.run(t, failed.ID); passed.Status != model.RunPassed {
		t.Fatalf("tag run after publication retry = %#v, want passed", passed)
	}
	if runs, _ := harness.fixture.store.ListRecentRuns(context.Background(), model.RunListFilter{RepoID: harness.fixture.repo.ID, Limit: 10}); len(runs) != 1 {
		t.Fatalf("runs = %d, want only the original tag run", len(runs))
	}
	if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": failed.ID, "force": true}); err == nil {
		t.Fatal("rerun of a published tag was accepted")
	}
	if len(harness.git.syncedTags) != 1 || harness.git.tagAttempts() != 2 {
		t.Fatalf("tag publications %v after %d attempts, want one delivered after two attempts", harness.git.syncedTags, harness.git.tagAttempts())
	}
}

func TestRunRerunPolicy(t *testing.T) {
	branch := model.Run{ID: "b", RefKind: model.RefBranch, Trigger: "branch"}
	schedule := model.Run{ID: "s", RefKind: model.RefBranch, Trigger: "schedule"}
	tag := model.Run{ID: "t", RefKind: model.RefTag, Trigger: "tag", Release: true}
	for name, test := range map[string]struct {
		run          model.Run
		admin, force bool
		want         error
	}{
		"uplink branch":      {run: branch},
		"uplink schedule":    {run: schedule, want: ErrForbidden},
		"uplink tag":         {run: tag, force: true, want: ErrForbidden},
		"admin schedule":     {run: schedule, admin: true},
		"admin tag":          {run: tag, admin: true, want: ErrInvalidInput},
		"admin tag forced":   {run: tag, admin: true, force: true},
		"promotion":          {run: model.Run{Trigger: "promotion", RefKind: model.RefBranch}, admin: true, want: ErrInvalidInput},
		"apply":              {run: model.Run{Trigger: "apply", RefKind: model.RefBranch}, admin: true, force: true, want: ErrInvalidInput},
		"plan by an uplink":  {run: model.Run{Trigger: "plan", RefKind: model.RefBranch}, want: ErrInvalidInput},
		"admin branch force": {run: branch, admin: true, force: true},
	} {
		_, err := RunRerunPolicy(test.run, test.admin, test.force, false)
		if (test.want == nil && err != nil) || (test.want != nil && !errors.Is(err, test.want)) {
			t.Errorf("%s: error = %v, want %v", name, err, test.want)
		}
	}
}

// A tag whose rerun passed every release step and failed only in publishing
// is not executed a third time: rerunning the original is refused and points
// at publish_retry of the rerun, which publishes the tag once.
func TestTagRerunRefusesAThirdExecutionAfterAPublishingFailure(t *testing.T) {
	harness := newRerunHarness(t, infraFailure, greenResult)
	original := admitRerunTag(t, harness)
	if original.Status != model.RunFailed || original.Phase == "publishing" {
		t.Fatalf("original tag run = %#v, want an infrastructure failure", original)
	}
	harness.git.failTags = 1
	second := harness.rerun(t, rerunAdmin, map[string]any{"id": original.ID, "force": true})
	harness.process(t)
	if run := harness.run(t, second.RunID); run.Status != model.RunFailed || run.Phase != "publishing" {
		t.Fatalf("tag rerun = %#v, want failed/publishing", run)
	}
	_, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": original.ID, "force": true})
	if !errors.Is(err, store.ErrInvalidState) || !strings.Contains(err.Error(), "publish_retry") || !strings.Contains(err.Error(), second.RunID) {
		t.Fatalf("third execution error = %v, want ErrInvalidState pointing at publish_retry of %s", err, second.RunID)
	}
	retried := harness.rerun(t, rerunAdmin, map[string]any{"id": second.RunID, "force": true})
	if retried.Mode != "publish_retry" || retried.Status != string(model.PublicationDelivered) {
		t.Fatalf("rerun of the publishing failure = %#v, want a delivered publication retry", retried)
	}
	harness.fixture.jobs.mu.Lock()
	executions := len(harness.fixture.jobs.createdReleases)
	harness.fixture.jobs.mu.Unlock()
	if executions != 2 || len(harness.git.syncedTags) != 1 || harness.git.tagAttempts() != 2 {
		t.Fatalf("release executions %d, tag publications %v after %d attempts; want 2 executions and one delivered tag",
			executions, harness.git.syncedTags, harness.git.tagAttempts())
	}
}
