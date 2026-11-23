package app_test

import (
	"context"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestManualWorkflowUsesNormalAdmissionAndRetainsFile(t *testing.T) {
	f := newFixture(t)
	const file = ".oberth/walk.yaml"
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		if scope.File != file || scope.Release {
			t.Fatal("manual workflow escaped CI scope", scope)
		}
		return load(ctx, scope)
	}
	read := f.source.Read
	f.source.Read = func(ctx context.Context, rev gitport.Revision, path string) (gitport.File, error) {
		if path != file || rev.Commit != request().Ref.CommitSHA {
			t.Fatal("manual file was not read from pushed commit")
		}
		return read(ctx, rev, path)
	}
	s := f.new(t)
	r := receive()
	r.Source, r.Updates[0].Run, r.Updates[0].Request.File = f.source, true, file
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	a, ok := f.runs.Admission("r1")
	if !ok {
		t.Fatal("manual run was not retained")
	}
	v := a.Prepared.View()
	if v.SelectedFile != file || v.SourceReceipt.File != file || v.SourceReceipt.Revision.Commit != request().Ref.CommitSHA || a.Release {
		t.Fatal("manual admission lost its exact source or tier")
	}
	assertManualRerun(t, f, s, v)
}

func TestManualWorkflowRefusesWithoutAuthorityOrFile(t *testing.T) {
	for _, change := range []string{"no request", "deleted", "missing", "secret", "identity", "resource", "bad image", "default release", "routed release", "tag", "traversal"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			r := receive()
			r.Updates[0].Run, r.Updates[0].Request.File = true, ".oberth/walk.yaml"
			changeManualFile(f, &r.Updates[0], change)
			r.Source = f.source
			s := f.new(t)
			_, err := s.Gate(t.Context(), r)
			if err == nil || f.creates.Load() != 0 || saved(f, r.ID) {
				t.Fatal("manual workflow bypassed admission", err)
			}
			if strings.Contains(change, "release") && !strings.Contains(err.Error(), "release files require an admin tag") {
				t.Fatal("release refusal has no clear reason", err)
			}
		})
	}
}

// A source file is selected explicitly; it is never discovered beside build.yaml.
func TestOrdinaryPushStillSelectsBuild(t *testing.T) {
	f := newFixture(t)
	read := f.source.Read
	f.source.Read = func(ctx context.Context, rev sourcecontract.Revision, file string) (gitport.File, error) {
		if file != workflowpolicy.BuildFile {
			t.Fatal("ordinary push discovered another workflow")
		}
		return read(ctx, rev, file)
	}
	f.gate(t, f.new(t))
}

func changeManualFile(f *fixture, u *app.RefUpdate, change string) {
	switch change {
	case "no request":
		u.Run = false
	case "deleted":
		u.Deleted = true
	case "missing":
		f.source.Read = missingFile
	case "default release":
		u.Request.File = workflowpolicy.ReleaseFile
	case "routed release":
		f.d.Routes = workflowpolicy.ReleaseRoutes{request().Repository: {{Pattern: "v*", File: ".oberth/walk.yaml"}}}
	case "tag":
		u.Request.Ref.Name = "refs/tags/ordinary"
	case "traversal":
		u.Request.File = "../walk.yaml"
	default:
		changeManualDocument(f, change)
	}
}

func changeManualDocument(f *fixture, change string) {
	w := workflow()
	switch change {
	case "secret":
		w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "private/value"}
		addLogin(w)
	case "identity":
		addLogin(w)
	case "resource":
		w.Spec.Templates[0].Container = nil
		w.Spec.Templates[0].Resource = &wfv1.ResourceTemplate{Action: "create", Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ungranted, namespace: runs}\n"}
	case "bad image":
		w.Spec.Templates[0].Container.Image = "unapproved/image:1"
	}
	f.setDocument(w)
}

func assertManualRerun(t *testing.T, f *fixture, s *app.Submitter, v admissioncontract.PreparedView) {
	t.Helper()
	q := request()
	q.RunID = "rerun"
	q.Original = &stateport.Run{Admission: v}
	rerun, err := s.Admit(t.Context(), q, f.source)
	if err != nil || rerun.View().SelectedFile != v.SelectedFile {
		t.Fatal("rerun changed selected workflow", err)
	}
}

func TestRejectedManualReplayRetainsFileForRerun(t *testing.T) {
	f := newFixture(t)
	const file = ".oberth/walk.yaml"
	read := f.source.Read
	f.source.Read = missingFile
	s := f.new(t)
	r := receive()
	r.Updates[0].Run, r.Updates[0].Request.File = true, file
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	old, err := f.runs.Run(t.Context(), "r1")
	if err != nil || old.HasAdmission || old.Status != stepmodel.RunFailed || old.Origin.RequestedFile != file {
		t.Fatal("rejected replay lost requested file", old.Origin, err)
	}
	f.source.Read = func(ctx context.Context, rev gitport.Revision, path string) (gitport.File, error) {
		if path != file {
			t.Fatal("rejected workflow rerun selected another file", path)
		}
		return read(ctx, rev, path)
	}
	q := request()
	q.RunID, q.Original = "rerun", &old
	rerun, err := s.Admit(t.Context(), q, f.source)
	if err != nil || rerun.View().SelectedFile != file {
		t.Fatal("rejected workflow rerun lost selector", err)
	}
	f.d.Routes = workflowpolicy.ReleaseRoutes{q.Repository: {{Pattern: "v*", File: file}}}
	if _, err := f.new(t).Admit(t.Context(), q, f.source); err == nil {
		t.Fatal("retained selector bypassed current release routing")
	}
}
