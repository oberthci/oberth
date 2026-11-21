package cronsync_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
)

type fixture struct {
	config                                        cronsync.Config
	t                                             *testing.T
	c                                             *cronsync.Syncer
	native                                        *fake.Native
	git                                           *fake.Git
	now                                           time.Time
	repo                                          cronsync.Repository
	mu                                            sync.Mutex
	repos                                         []cronsync.Repository
	calls                                         []cronsync.Preparation
	paused                                        bool
	pauseErr, admitErr, validationErr, catalogErr error
	hook                                          fake.Hook
	gitHook                                       fake.Hook
	admitHook                                     func(context.Context, cronsync.Preparation) error
	validationCalls                               int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), repo: cronsync.Repository{ID: 7, Qualified: "origin/team/widget", DefaultBranch: "main"}}
	f.repos = []cronsync.Repository{f.repo}
	f.native = fake.NewNative("ci", func() time.Time { return f.now },
		func(ctx context.Context, verb, name string) error {
			f.mu.Lock()
			hook := f.hook
			f.mu.Unlock()
			if hook != nil {
				return hook(ctx, verb, name)
			}
			return nil
		})
	f.git = fake.NewGit(func(ctx context.Context, verb, name string) error {
		f.mu.Lock()
		hook := f.gitHook
		f.mu.Unlock()
		if hook != nil {
			return hook(ctx, verb, name)
		}
		return nil
	})
	config := cronsync.Config{InstanceID: "frontdoor", Namespace: "ci", Client: f.native, Git: f.git, Now: func() time.Time { return f.now }, Admit: f.admit, Repositories: f.repositories, Repository: f.repository, PauseIntent: f.pause, ValidateNativeSchema: f.validate}
	config.RetainAdmission = func(context.Context, *admissioncontract.PreparedAdmission) error { return nil }
	config.PublishAdmission = func(context.Context, int64, string, []string) error { return nil }
	config.ForgetAdmission = func(context.Context, int64, string) error { return nil }
	config.PruneAdmissions = func(context.Context, int64, []string) error { return nil }
	f.config = config
	var err error
	f.c, err = cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.put(f.repo, "schedules:\n- name: nightly\n  cron: '0 3 * * *'\n")
	return f
}
func (f *fixture) repositories(context.Context) ([]cronsync.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cronsync.Repository(nil), f.repos...), f.catalogErr
}
func (f *fixture) repository(ctx context.Context, key string) (cronsync.Repository, bool, error) {
	repos, err := f.repositories(ctx)
	if err != nil {
		return cronsync.Repository{}, false, err
	}
	for _, repo := range repos {
		if repo.Qualified == key {
			return repo, true, nil
		}
	}
	return cronsync.Repository{}, false, nil
}
func (f *fixture) pause(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused, f.pauseErr
}
func (f *fixture) validate(context.Context, *wfv1.CronWorkflow, []admissioncontract.TemplateSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validationCalls++
	return f.validationErr
}
func (f *fixture) admit(ctx context.Context, request cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	err, hook := f.admitErr, f.admitHook
	f.mu.Unlock()
	if hook != nil {
		if failure := hook(ctx, request); failure != nil {
			return nil, failure
		}
	}
	if err != nil {
		return nil, err
	}
	return admissioncontract.Seal(prepared(request))
}
func prepared(request cronsync.Preparation) admissioncontract.PreparedView {
	labels := map[string]string{nativebinding.EngineLabel: "argo", nativebinding.TriggerLabel: "ci", nativebinding.TierLabel: "ci", nativebinding.CommitLabel: request.Revision.CommitSHA}
	labels[nativebinding.RepositoryLabel] = strconv.FormatInt(request.Repository.ID, 10)
	annotations := map[string]string{nativebinding.RefAnnotation: request.Revision.Name}
	spec := wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "image@sha256:" + strings.Repeat("c", 64), Command: []string{"true"}}}},
		WorkflowMetadata: &wfv1.WorkflowMetadata{
			LabelsFrom: map[string]wfv1.LabelValueFrom{nativebinding.RunLabel: {Expression: "workflow.name"}},
		},
	}
	for _, fact := range [][2]string{{"oberth-schedule", request.Entry.Name}, {"oberth-trigger", "schedule"}, {"oberth-ref", request.Revision.Name}, {"oberth-sha", request.Revision.CommitSHA}} {
		spec.Arguments.Parameters = append(spec.Arguments.Parameters, wfv1.Parameter{Name: fact[0], Value: wfv1.AnyStringPtr(fact[1])})
	}
	return admissioncontract.PreparedView{
		Workflow:      &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Labels: labels, Annotations: annotations}, Spec: spec},
		Repository:    request.Repository.Qualified,
		Ref:           request.Revision.Name,
		ObjectSHA:     request.Revision.ObjectSHA,
		CommitSHA:     request.Revision.CommitSHA,
		Trigger:       model.TriggerSchedule,
		ScheduleEntry: request.Entry.Name,
		Plan:          admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "main", Name: "main"}}},
	}
}
func (f *fixture) put(repo cronsync.Repository, raw string) {
	ref := gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)}
	f.git.PutRef(repo.Qualified, ref)
	f.git.PutFile(gitport.Revision{Repository: repo.Qualified, Commit: ref.CommitSHA}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("b", 40), Bytes: []byte(raw)})
}
func (f *fixture) object(repo cronsync.Repository, entry string) *wfv1.CronWorkflow {
	return f.objectAt(f.t.Context(), repo, entry)
}
func (f *fixture) objectAt(ctx context.Context, repo cronsync.Repository, entry string) *wfv1.CronWorkflow {
	f.t.Helper()
	name, err := nativebinding.CronName(repo.ID, entry)
	if err != nil {
		f.t.Fatal(err)
	}
	object, err := f.native.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return object
}
func (f *fixture) sync() {
	f.t.Helper()
	if err := f.c.SyncAll(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func assertOrphanDeletion(t *testing.T, action fake.Action, expected *wfv1.CronWorkflow) {
	t.Helper()
	if action.Verb != "delete" || action.Name != expected.Name || action.Delete.Preconditions == nil || action.Delete.Preconditions.UID == nil || *action.Delete.Preconditions.UID != expected.UID || action.Delete.PropagationPolicy == nil || *action.Delete.PropagationPolicy != metav1.DeletePropagationOrphan {
		t.Fatal("deletion must orphan the observed UID", action)
	}
}
