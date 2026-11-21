package argojob_test

import (
	"context"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/store"
)

//nolint:gochecknoglobals // Construct before tests; the native constructor mutates process-wide initialization state.
var childFixtureLogger = argologging.NewSlogLogger(argologging.Warn, argologging.Text)

type childFixture struct {
	s        *store.Store
	path     string
	config   cronsync.Config
	syncer   *cronsync.Syncer
	native   *cronfake.Native
	parent   *wf.CronWorkflow
	child    *wf.Workflow
	prepared *admissioncontract.PreparedAdmission
	kept     keptRoots
}

func newChildFixture(t *testing.T) *childFixture {
	t.Helper()
	s, path := compositionStorePath(t)
	registerCompositionRepository(t, s, "origin/team/repo")
	repo, err := s.Repository(t.Context(), "origin/team/repo", nil)
	compositionOK(t, err)
	g := cronfake.NewGit(nil)
	ref := gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)}
	g.PutRef(repo.Canonical, ref)
	g.PutFile(gitport.Revision{Repository: repo.Canonical, Commit: ref.CommitSHA}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("b", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
	native := cronfake.NewNative("ci", compositionClock, nil)
	f := &childFixture{s: s, path: path, native: native}
	repository := cronsync.Repository{ID: repo.ID, Qualified: repo.Canonical, DefaultBranch: "main"}
	f.config = cronsync.Config{Namespace: "ci", Client: native, Git: g, Now: compositionClock,
		Repositories:         func(context.Context) ([]cronsync.Repository, error) { return []cronsync.Repository{repository}, nil },
		Repository:           func(context.Context, string) (cronsync.Repository, bool, error) { return repository, true, nil },
		PauseIntent:          func(context.Context, string, string) (bool, error) { return false, nil },
		RetainAdmission:      s.RetainSchedule,
		PublishAdmission:     s.PublishSchedule,
		ForgetAdmission:      s.ForgetSchedule,
		PruneAdmissions:      s.PruneSchedules,
		ValidateNativeSchema: func(context.Context, *wf.CronWorkflow, []admissioncontract.TemplateSnapshot) error { return nil },
		Admit: func(_ context.Context, r cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
			f.prepared, err = admissioncontract.Seal(childAdmission(r))
			return f.prepared, err
		}}
	f.syncer, err = cronsync.New(f.config)
	compositionOK(t, err)
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	name, err := nativebinding.CronName(repo.ID, "nightly")
	compositionOK(t, err)
	f.parent, err = native.Get(t.Context(), name, meta.GetOptions{})
	compositionOK(t, err)
	f.child = f.fire(t)
	return f
}

func childAdmission(r cronsync.Preparation) admissioncontract.PreparedView {
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Namespace: "ci", Labels: map[string]string{
		nativebinding.EngineLabel: "argo", nativebinding.TriggerLabel: "ci", nativebinding.TierLabel: "ci", nativebinding.CommitLabel: r.Revision.CommitSHA, nativebinding.RepositoryLabel: strconv.FormatInt(r.Repository.ID, 10)}, Annotations: map[string]string{nativebinding.RefAnnotation: r.Revision.Name}}, Spec: wf.WorkflowSpec{
		Entrypoint: "build", Templates: []wf.Template{{Name: "build", Container: &core.Container{Image: "image", Command: []string{"true"}}}},
		WorkflowMetadata: &wf.WorkflowMetadata{LabelsFrom: map[string]wf.LabelValueFrom{nativebinding.RunLabel: {Expression: "workflow.name"}}}}}
	for _, p := range [][2]string{{"oberth-schedule", r.Entry.Name}, {"oberth-trigger", "schedule"}, {"oberth-ref", r.Revision.Name}, {"oberth-sha", r.Revision.CommitSHA}} {
		w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, wf.Parameter{Name: p[0], Value: wf.AnyStringPtr(p[1])})
	}
	return admissioncontract.PreparedView{Workflow: w, Repository: r.Repository.Qualified, Ref: r.Revision.Name, ObjectSHA: r.Revision.ObjectSHA, CommitSHA: r.Revision.CommitSHA, Trigger: model.TriggerSchedule, ScheduleEntry: r.Entry.Name,
		Identity: admissioncontract.IdentityEvidence{Namespace: "ci"}, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}}
}

func (f *childFixture) fire(t *testing.T) *wf.Workflow {
	t.Helper()
	ctx := argologging.WithLogger(t.Context(), childFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, f.parent, f.parent.Name+"-123", compositionClock())
	child.Namespace, child.UID = "ci", "child-uid"
	child.ResourceVersion = "1"
	child.CreationTimestamp = meta.NewTime(compositionClock())
	f.native.TrackChild(child)
	return child
}

func orphanOptions() meta.DeleteOptions {
	policy := meta.DeletePropagationOrphan
	return meta.DeleteOptions{PropagationPolicy: &policy}
}
func (f *childFixture) executor() *argojob.Executor { return argojob.New(f.executorConfig()) }
func (f *childFixture) executorConfig() argojob.Config {
	return argojob.Config{Runs: f.s, ChildRuns: f.s, Keeper: &f.kept, Children: f.syncer, Progress: f.s, Now: compositionClock,
		Workflows: &nativefake.Workflows{Object: f.child}, Cache: &nativefake.Cache{WorkflowObject: f.child}}
}

// keptRoots records each Keep call as its owner and bindings.
type keptRoots struct {
	mu    sync.Mutex
	calls map[string][]sourcecontract.Binding
}

func (k *keptRoots) Keep(_ context.Context, owner string, bindings []sourcecontract.Binding) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.calls == nil {
		k.calls = map[string][]sourcecontract.Binding{}
	}
	k.calls[owner] = bindings
	return nil
}
func (k *keptRoots) owners() map[string][]sourcecontract.Binding {
	k.mu.Lock()
	defer k.mu.Unlock()
	return maps.Clone(k.calls)
}
