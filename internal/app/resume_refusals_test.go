package app_test

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestResumeChangedAdmissionHasNoEffect(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"definition", "grant input", "grant revision", "revoked grant", "per actor grant"} {
		t.Run(scenario, func(t *testing.T) { checkChangedResume(t, scenario) })
	}
}

func checkChangedResume(t *testing.T, scenario string) {
	t.Helper()
	f, old, selection, w := resumeGrantFixture(t)
	a := request().Actor
	switch scenario {
	case "definition":
		w.Spec.Templates[0].Container.Command = []string{"false"}
		f.setDocument(w)
		f.d.Source = f.source
	case "grant input":
		selection.path = "other"
	case "grant revision":
		selection.revision = "2"
	case "revoked grant":
		selection.allow = false
	case "per actor grant":
		a.Identity = "other"
	}
	n := &resumeNativeStub{Native: f.native, runs: f.runs}
	f.d.Native = n
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, q service.StartRequest) (app.Request, error) {
		r, e := resolveStart(ctx, q)
		r.RunID = "r2"
		return r, e
	}}
	s, err := app.New(f.d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	_, err = s.ResumeRun(t.Context(), service.ResumeRequest{Run: old, Actor: a, ID: "resume", Commit: strings.Repeat("f", 40)})
	if err == nil || n.applied {
		t.Fatal("changed admission resumed", scenario, err)
	}
	if _, err = f.runs.Run(t.Context(), "r2"); err == nil {
		t.Fatal("refusal inserted successor")
	}
}

type resumeGrantSelection struct {
	allow          bool
	revision, path string
}

func resumeGrantFixture(t *testing.T) (*fixture, stateport.Run, *resumeGrantSelection, *wf.Workflow) {
	t.Helper()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Container.EnvFrom = []core.EnvFromSource{{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "private"}}}}
	f.setDocument(w)
	selection := &resumeGrantSelection{allow: true, revision: "1", path: "private"}
	base := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		q, p, e := base(ctx, scope)
		if selection.allow && scope.Actor.Identity == request().Actor.Identity {
			q.Grants = []admissioncontract.GrantEvidence{{Repository: request().Repository, File: ".oberth/build.yaml", Step: "build", Kind: "cluster-secret", Path: selection.path, Revision: selection.revision, State: admissioncontract.StateVerified}}
		}
		return q, p, e
	}
	s := f.new(t)
	_, err := s.StartRun(t.Context(), startRequest("original", request()))
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.runs.Run(t.Context(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.runs.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: old.ID, Revision: old.Revision, Status: stepmodel.RunFailed}); err != nil {
		t.Fatal(err)
	}
	old, err = f.runs.Run(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, old, selection, w
}
