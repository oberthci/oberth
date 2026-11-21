package argojob_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This composes real Git, preparation, native conversion and durable custody.
// It observes a native child; it does not execute a step or fire a schedule.
func TestRealSchedulePreparationRecordsChild(t *testing.T) {
	t.Parallel()
	f := realScheduleFixture(t)
	assertScheduleFactContract(t, f)
	retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
	compositionOK(t, err)
	if !reflect.DeepEqual(retained.Workflow.Spec, f.parent.Spec.WorkflowSpec) || !reflect.DeepEqual(f.child.Spec, retained.Workflow.Spec) {
		t.Fatal("retained admission, published parent and native child differ")
	}
	for range 3 {
		compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	}
	assertScheduleRecord(t, f, retained)
}

func assertScheduleRecord(t *testing.T, f *childFixture, retained admissioncontract.PreparedView) {
	t.Helper()
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	compositionOK(t, err)
	events, err := f.s.Events(t.Context(), stateport.EventFilter{Limit: 10})
	compositionOK(t, err)
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	run, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if len(runs) != 1 || len(events.Events) != 1 || run.Origin.Actor != "oberth:scheduler" || run.Origin.ScheduleEntry != "nightly" || run.Origin.WorkflowUID != string(f.child.UID) || run.CommitSHA == nil || *run.CommitSHA != retained.CommitSHA || proof.Run.SpecSHA256 != retained.SpecDigest {
		t.Fatal("scheduled child was not recorded exactly once", runs, proof)
	}
	if run.Admission.SourceReceipt != retained.SourceReceipt || !reflect.DeepEqual(run.Admission.Plan, retained.Plan) {
		t.Fatal("record lost admitted Git provenance or plan")
	}
}

func (f *childFixture) configRepositoryID(t *testing.T) int64 {
	t.Helper()
	repo, err := f.s.Repository(t.Context(), gittest.Repository, nil)
	compositionOK(t, err)
	return repo.ID
}

func realScheduleFixture(t *testing.T, policies ...argoworkflow.Policy) *childFixture {
	t.Helper()
	return realScheduleDocument(t, scheduleWorkflow, policies...)
}

func realScheduleDocument(t *testing.T, document string, policies ...argoworkflow.Policy) *childFixture {
	t.Helper()
	f, commit := unpublishedScheduleFixture(t, document, policies...)
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	name, err := nativebinding.CronName(f.configRepositoryID(t), "nightly")
	compositionOK(t, err)
	f.parent, err = f.native.Get(t.Context(), name, meta.GetOptions{})
	compositionOK(t, err)
	if f.prepared == nil || f.prepared.View().CommitSHA != commit {
		t.Fatal("preparation did not select the real Git head")
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	compositionOK(t, err)
	if len(runs) != 0 {
		t.Fatal("parent preparation accepted a run")
	}
	assertScheduleSpecUnchanged(t, f.prepared.View().Workflow)
	f.child = f.fire(t)
	return f
}

func unpublishedScheduleFixture(t *testing.T, document string, policies ...argoworkflow.Policy) (*childFixture, string) {
	t.Helper()
	git := gittest.New(t)
	git.Write(t, ".oberth/build.yaml", []byte(document))
	git.Write(t, schedule.FileName, []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]\n"))
	git.Commit = git.CommitFile(t, "scheduled workflow")
	git.Git(t, git.Work, "push", git.Bare, "HEAD:refs/heads/main")
	cache := git.Cache(t, nil)
	s, path := compositionStorePath(t)
	registerCompositionRepository(t, s, gittest.Repository)
	repo, err := s.Repository(t.Context(), gittest.Repository, nil)
	compositionOK(t, err)
	native := cronfake.NewNative("ci", compositionClock, nil)
	f := &childFixture{s: s, path: path, native: native}
	preparer := scheduleSubmitter(t, cache, s, policies...)
	repository := cronsync.Repository{ID: repo.ID, Qualified: repo.Canonical, DefaultBranch: "main"}
	f.config = cronsync.Config{Namespace: "ci", Client: native, Git: cache, Now: compositionClock,
		Repositories:    func(context.Context) ([]cronsync.Repository, error) { return []cronsync.Repository{repository}, nil },
		Repository:      func(context.Context, string) (cronsync.Repository, bool, error) { return repository, true, nil },
		PauseIntent:     func(context.Context, string, string) (bool, error) { return false, nil },
		RetainAdmission: s.RetainSchedule, PublishAdmission: s.PublishSchedule, ForgetAdmission: s.ForgetSchedule, PruneAdmissions: s.PruneSchedules,
		ValidateNativeSchema: func(ctx context.Context, p *wf.CronWorkflow, snapshots []admissioncontract.TemplateSnapshot) error {
			return argoworkflow.ValidateNativeSchema(ctx, &wf.Workflow{Spec: p.Spec.WorkflowSpec}, admissioncontract.PrepareRequest{Templates: snapshots})
		},
		Admit: func(ctx context.Context, r cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
			request := app.Request{RepositoryID: r.Repository.ID, Repository: r.Repository.Qualified, Ref: r.Revision, ScheduleEntry: r.Entry.Name}
			request.Actor.Identity, request.Actor.AuthMethod = "schedule", "system"
			f.prepared, err = preparer.PrepareSchedule(ctx, request)
			return f.prepared, err
		},
	}
	f.syncer, err = cronsync.New(f.config)
	compositionOK(t, err)
	return f, git.Commit
}

// Only preparation is reachable here. Calls to any run-start dependency fail.
type scheduleUnused struct {
	app.Service
	app.Keeper
	app.StartResolver
}
type scheduleAuthority struct {
	pipeline string
	policy   argoworkflow.Policy
}

func (a scheduleAuthority) Load(_ context.Context, _ app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
	return admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: a.pipeline}}, a.policy, nil
}

func scheduleSubmitter(t *testing.T, source app.Source, runs app.Runs, policies ...argoworkflow.Policy) *app.Submitter {
	t.Helper()
	pipeline, err := identityname.Name(identityname.Key{Repository: gittest.Repository}, identityname.PipelineCI)
	compositionOK(t, err)
	executor := argojob.New(argojob.Config{Accounts: nativefake.Accounts{Names: map[string]string{pipeline: "pipeline-uid"}}})
	unused := &scheduleUnused{}
	var policy argoworkflow.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	s, err := app.New(app.Deps{Lifetime: t.Context(), SourceTimeout: time.Minute, PublicURL: "https://oberth.ci", Source: source,
		Authority: scheduleAuthority{pipeline, policy}, Native: executor, Runs: runs, Service: unused, Keeper: unused, Resolver: unused, Deadline: context.WithTimeout})
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, s.Close(context.Background())) })
	return s
}

const scheduleWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  labels: {pipeline: authored}
spec:
  entrypoint: build
  workflowMetadata:
    labels: {authored: workflow}
    annotations: {note: workflow}
  podMetadata:
    labels: {authored: pod}
    annotations: {note: pod}
  templates:
  - name: build
    container:
      image: oberth.ci/build:1
      command: ["true"]
`
