package contracttest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// RunPort is the narrow owner contract exercised by service stores and fakes.
type RunPort interface {
	AcceptBatch(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error)
	AcceptedRuns(context.Context, string) ([]stateport.Run, error)
	Run(context.Context, string) (stateport.Run, error)
	RunState(context.Context, string) (stateport.RunState, error)
	ResolveRun(context.Context, []string, string) (stateport.Run, error)
	RunByCommit(context.Context, []string, string) (stateport.Run, error)
	RunByRef(context.Context, []string, string) (stateport.Run, error)
	Runs(context.Context, stateport.RunFilter) ([]stateport.Run, error)
	RecentRuns(context.Context, []string) (map[string][]stateport.RunMark, error)
	Steps(context.Context, string) ([]stepmodel.RunStep, error)
	SaveSteps(context.Context, string, []stepmodel.RunStep) error
	BeginPurge(context.Context, stateport.Action, string, string) error
	FinishPurge(context.Context, stateport.Action, string, string) error
	PurgeReceipt(context.Context, string) (stateport.PurgeReceipt, error)
	Finish(context.Context, stateport.Action, stateport.RunUpdate) error
	Cancel(context.Context, stateport.Action, stateport.RunCancellation) (stateport.Obligation, error)
	Obligations(context.Context, string) ([]stateport.Obligation, error)
	Events(context.Context, stateport.EventFilter) (model.EventPage, error)
	Changes() <-chan struct{}
	RepositoryScope(context.Context, []string) ([]string, error)
	Repository(context.Context, string, []string) (stateport.Repository, error)
	Repositories(context.Context, stateport.CatalogFilter) ([]stateport.Repository, error)
}

// RunFixture supplies preparation and authority facts independently of storage.
type RunFixture struct {
	Port          RunPort
	Admit         func(*testing.T, string, model.CIRequest) *admissioncontract.PreparedAdmission
	Repository    func(*testing.T, string)
	Archive       func(*testing.T, string)
	NativeAdvance func(*testing.T, string)
	Nodes         func(*testing.T, string, []stateport.Node)
	Prune         func(*testing.T)
	Audit         func(*testing.T) []model.AuditAction
	Publication   func(context.Context, string) (model.Publication, error)
	Emit          func(context.Context, []stateport.EventWrite) error
}

type RunCase struct {
	ID, Repo, Ref, Actor, Commit, Object string
	Trigger                              model.RunTrigger
}

// RunSuite runs unchanged against each real or in-memory consumer store.
func RunSuite(t *testing.T, factory func(*testing.T) RunFixture) {
	for name, check := range map[string]func(*testing.T, RunFixture){
		"plan_step_shape":      checkPlanStepShape,
		"native_step_shape":    checkNativeStepShape,
		"run_start":            checkRunStart,
		"malformed_acceptance": checkMalformedAcceptance,
		"progress_finish":      checkProgressFinish,
		"initial_events":       checkInitialEvents,
		"acceptance_rollback":  checkAcceptanceRollbackResult,
		"sealed_decision":      checkSealedDecision,
		"rejected_rerun":       checkRejectedAndRerun,
		"stale_verdict":        checkStaleVerdict,
		"purge":                checkRunPurge,
		"acceptance":           checkRunAcceptance,
		"queries":              checkRunQueries,
		"latest_cursor":        checkLatestCursor,
		"latest_repository":    checkLatestRepository,
		"recent_runs":          checkRecentRuns,
		"commit_prefix":        checkCommitPrefix,
		"scope_catalog":        checkScopeCatalog,
		"cancel":               checkCancellation,
		"cancel_race":          checkCancellationRace,
		"wakeups":              checkCommitWakeups,
		"holds_verdict":        checkHoldsAndVerdict,
		"events":               checkRunEvents,
		"mixed_receive":        checkMixedReceive,
	} {
		t.Run(name, func(t *testing.T) { check(t, factory(t)) })
	}
}

func batchFor(t *testing.T, f RunFixture, c RunCase) stateport.BatchAcceptance {
	t.Helper()
	if c.Repo == "" {
		c.Repo = "origin/team/repo"
	}
	if c.Ref == "" {
		c.Ref = "refs/heads/main"
	}
	if c.Commit == "" {
		c.Commit = strings.Repeat("a", 40)
	}
	if c.Object == "" {
		c.Object = c.Commit
	}
	if c.Actor == "" {
		c.Actor = "operator"
	}
	if c.Trigger == "" {
		c.Trigger = model.TriggerBranch
	}
	r := model.CIRequest{ReceiveID: "receive-" + c.ID, Repo: c.Repo, Ref: c.Ref, ObjectSHA: c.Object, CommitSHA: new(c.Commit), Actor: actor.Actor{Identity: c.Actor, AuthMethod: "cert"}, Trigger: c.Trigger}
	p := f.Admit(t, c.ID, r)
	return stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}}
}

func acceptOneRun(t *testing.T, f RunFixture, c RunCase) stateport.Run {
	t.Helper()
	recorded, err := f.Port.AcceptBatch(t.Context(), batchFor(t, f, c))
	rows, fresh := recorded.Runs, recorded.Fresh
	mustRun(t, err)
	if !fresh || len(rows) != 1 {
		t.Fatal("accept did not create one run", rows, fresh)
	}
	return rows[0]
}

func mustRun(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func runCode(t *testing.T, err error, code publicerr.Code) {
	t.Helper()
	if err == nil || publicerr.Classify(err).Code() != code {
		t.Fatal("wrong refusal", err, code)
	}
}
func unchangedSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("failed or unchanged transaction signaled")
	default:
	}
}
func changedSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("committed fact did not signal")
	}
}
func runEvent(r stateport.Run, key string) stateport.EventWrite {
	return stateport.EventWrite{Event: model.AgentEvent{Repo: r.Repo, RunID: r.ID, Kind: string(model.EventRunUpdated), At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Summary: key}, Dedupe: key}
}
