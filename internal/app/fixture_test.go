package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fixture struct {
	d                 app.Deps
	source            fake.Source
	authority         fake.Authority
	native            fake.Native
	runs              *runFixture
	keeper            *fake.Keeper
	prepares, creates atomic.Int32
}

func request() app.Request {
	return app.Request{RepositoryID: 1, Repository: "origin/team/widget", RunID: "r1", Ref: sourcecontract.Ref{Name: "refs/heads/topic", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("b", 40)}, Trigger: model.TriggerBranch, Actor: actor.Actor{AuthMethod: "key", Identity: "developer"}}
}
func workflow() *wfv1.Workflow {
	return &wfv1.Workflow{TypeMeta: metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow"}, Spec: wfv1.WorkflowSpec{Entrypoint: "build", Templates: []wfv1.Template{{Name: "build", Container: &corev1.Container{Image: "oberth.ci/build:1", Command: []string{"true"}}}}}}
}
func document(w *wfv1.Workflow) []byte {
	raw, err := json.Marshal(w)
	if err != nil {
		panic(err)
	}
	return raw
}
func newFixture(t *testing.T) *fixture {
	f := &fixture{runs: &runFixture{Store: realStore(t), t: t, inputs: map[string]stateport.Acceptance{}}, keeper: &fake.Keeper{}}
	f.source = newSource()
	f.authority = fake.Authority{LoadFunc: func(_ context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		return admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "runs", PipelineAccount: "pipeline"}}, argoworkflow.Policy{Release: scope.Release, ImagePrefixes: []string{"oberth.ci/"}}, nil
	}}
	f.native = fake.Native{
		Hook: admissioncontract.PrepareHooks{LoginAudience: "login", VerifyIdentities: func(ctx context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
			f.prepares.Add(1)
			return admissioncontract.IdentityEvidence{Namespace: r.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: r.PipelineAccount, UID: "pipeline-uid"}, State: admissioncontract.StateVerified}, ctx.Err()
		}},
		CreateFunc: func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
			f.creates.Add(1)
			return binding(p.View()), ctx.Err()
		},
		FindFunc: func(ctx context.Context, run stateport.Run) (nativebinding.RunBinding, bool, error) {
			return binding(run.Admission), true, ctx.Err()
		},
		StopFunc:      func(ctx context.Context, _ nativebinding.RunBinding) error { return ctx.Err() },
		TerminateFunc: func(ctx context.Context, _ nativebinding.RunBinding) error { return ctx.Err() },
	}
	f.d = app.Deps{SourceTimeout: time.Minute, Lifetime: context.Background(), PublicURL: "https://oberth.ci", Source: f.source, Authority: f.authority, Runs: f.runs, Service: f.runs, Resolver: fake.StartResolver{Resolve: resolveStart}, Native: f.native, Keeper: f.keeper, Deadline: context.WithTimeout}
	return f
}
func object(v admissioncontract.PreparedView) nativebinding.Object {
	return nativebinding.Object{Namespace: "runs", Name: nativebinding.WorkflowName(v.RunID), UID: "uid-" + v.RunID}
}
func (f *fixture) new(t *testing.T) *app.Submitter {
	t.Helper()
	f.d.Source = f.source
	if f.native.BindFunc == nil {
		f.native.BindFunc = f.bindNative
	}
	f.d.Native = f.native
	f.d.Authority = f.authority
	s, err := app.New(f.d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}
func receive() app.Receive {
	return app.Receive{ID: "receive1", Updates: []app.RefUpdate{{Request: request()}}}
}
func (f *fixture) gate(t *testing.T, s *app.Submitter) app.Receive {
	t.Helper()
	r := receive()
	r.Source = f.source
	if _, err := s.Gate(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}
func mustAdmit(t *testing.T, s *app.Submitter, f *fixture) *admissioncontract.PreparedAdmission {
	t.Helper()
	p, err := s.Admit(context.Background(), request(), f.source)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func expectRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var e *app.Refusal
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s refusal, got %v", code, err)
	}
}
func batch(p *admissioncontract.PreparedAdmission, id string) stateport.BatchAcceptance {
	q := model.CIRequest{ReceiveID: id, Repo: request().Repository, Ref: p.View().Ref, ObjectSHA: p.View().ObjectSHA, CommitSHA: new(p.View().CommitSHA), Actor: request().Actor, Trigger: p.View().Trigger, ScheduleEntry: p.View().ScheduleEntry}
	return stateport.BatchAcceptance{ReceiveID: id, Repository: q.Repo, Actor: q.Actor, Runs: []stateport.Acceptance{{Request: q, Prepared: p}}}
}
func accepted(t *testing.T, f *fixture, p *admissioncontract.PreparedAdmission) {
	t.Helper()
	if _, err := f.runs.AcceptBatch(context.Background(), batch(p, "crash")); err != nil {
		t.Fatal(err)
	}
}
func saved(f *fixture, id string) bool {
	_, err := f.runs.AcceptedRuns(context.Background(), id)
	return err == nil
}
func missingFile(_ context.Context, _ gitport.Revision, path string) (gitport.File, error) {
	return gitport.File{}, &gitport.AbsentError{Path: path}
}
func (f *fixture) setDocument(w *wfv1.Workflow) {
	raw := document(w)
	f.source.Read = func(_ context.Context, _ gitport.Revision, path string) (gitport.File, error) {
		return gitport.File{Path: path, Blob: strings.Repeat("c", 40), Mode: 0100644, Bytes: append([]byte(nil), raw...)}, nil
	}
}
func addSource(w *wfv1.Workflow, d sourcecontract.Descriptor) {
	raw, _ := json.Marshal(d)
	mode := int32(0755)
	path := "/work/src"
	if d.Kind == sourcecontract.Pinned {
		path = "/work/sources/lib"
	}
	w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "source", Path: path, Mode: &mode, ArtifactLocation: wfv1.ArtifactLocation{Plugin: &wfv1.PluginArtifact{Name: "oberth", Key: argoworkflow.SourceArtifactKey, Configuration: string(raw)}}}}
}

func addLogin(w *wfv1.Workflow) {
	seconds := int64(600)
	t := &w.Spec.Templates[0]
	t.Volumes = []corev1.Volume{{Name: "login", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "login", ExpirationSeconds: &seconds, Path: "token"}}}}}}}
	t.Container.VolumeMounts = []corev1.VolumeMount{{Name: "login", MountPath: "/var/run/secrets/secretstore", ReadOnly: true}}
}

func duplicateRoot(t *testing.T, p *admissioncontract.PreparedAdmission) *admissioncontract.PreparedAdmission {
	t.Helper()
	v := p.View()
	v.Plan.Entries = append(v.Plan.Entries, v.Plan.Entries[0])
	sealed, err := admissioncontract.Seal(v)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func resolveStart(_ context.Context, q service.StartRequest) (app.Request, error) {
	r := request()
	r.Repository = q.Request.Repo
	r.Ref = sourcecontract.Ref{Name: q.Request.Ref, ObjectSHA: q.Request.ObjectSHA}
	if q.Request.CommitSHA != nil {
		r.Ref.CommitSHA = *q.Request.CommitSHA
	}
	r.Trigger, r.ScheduleEntry, r.Actor = q.Request.Trigger, q.Request.ScheduleEntry, q.Request.Actor.Clone()
	return r, nil
}
func startRequest(id string, r app.Request) service.StartRequest {
	return service.StartRequest{Request: model.CIRequest{ReceiveID: id, Repo: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: new(r.Ref.CommitSHA), Actor: r.Actor.Clone(), Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry}}
}
func decision(u service.RefUpdate, action string) service.Decision {
	if action == "" {
		action = "run"
	}
	trigger := model.TriggerBranch
	if strings.HasPrefix(u.Ref, "refs/tags/") {
		trigger = model.TriggerTag
	}
	return service.Decision{Action: action, Request: model.CIRequest{ReceiveID: u.ID, Repo: u.Repo, Ref: u.Ref, ObjectSHA: u.ObjectSHA, CommitSHA: new(u.CommitSHA), Actor: u.Actor.Clone(), Trigger: trigger}}
}

func binding(v admissioncontract.PreparedView) nativebinding.RunBinding {
	o := object(v)
	return nativebinding.RunBinding{Workflow: o, RepositoryID: 1, RunID: v.RunID, Revision: sourcecontract.Revision{Repository: v.Repository, Commit: v.CommitSHA}, Ref: v.Ref, Trigger: string(v.Trigger), Identity: v.Digest, SpecSHA256: v.SpecDigest}
}
func (f *fixture) bindNative(ctx context.Context, b nativebinding.RunBinding) error {
	store := f.d.Runs.(interface {
		Run(context.Context, string) (stateport.Run, error)
		BindWorkflow(context.Context, stateport.Action, nativebinding.RunBinding) error
	})
	r, err := store.Run(ctx, b.RunID)
	if err != nil {
		return err
	}
	return store.BindWorkflow(ctx, stateport.Action{Actor: r.Origin.Actor}, b)
}

func newSource() fake.Source {
	return fake.Source{
		Read: func(_ context.Context, r gitport.Revision, path string) (gitport.File, error) {
			w := workflow()
			w.Annotations = map[string]string{"test-commit": r.Commit}
			return gitport.File{Path: path, Blob: strings.Repeat("c", 40), Mode: 0100644, Bytes: document(w)}, nil
		},
		Export: func(_ context.Context, _ string, r sourcecontract.Ref) (sourcecontract.Export, error) {
			if r.Name == "" {
				r = sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("d", 40)}
			}
			return sourcecontract.Export{Ref: r, DefaultBranch: sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("d", 40)}}, nil
		},
		Reachable: func(context.Context, sourcecontract.Revision) (int64, bool, error) { return 2, true, nil },
	}
}
