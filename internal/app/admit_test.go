package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestAdmitReadsExactRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	p := mustAdmit(t, s, f)
	v := p.View()
	if v.Workflow.Annotations["test-commit"] != request().Ref.CommitSHA || v.ObjectSHA != request().Ref.ObjectSHA {
		t.Fatal("wrong exact revision", v)
	}
	if v.SourceReceipt.DocumentDigest == "" || v.Workflow.Name != "" {
		t.Fatal("unbound submission", v)
	}
}
func TestPreparedSubmissionUsesSealedSpec(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	f.setDocument(w)
	defaults := []byte(`{"spec":{"templateDefaults":{"container":{"image":"oberth.ci/build:1","workingDir":"/tmp/build"}}}}`)
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		q, p, err := load(ctx, scope)
		q.Defaults = runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: defaults, Digest: sourcecontract.Digest(defaults)}
		return q, p, err
	}
	var submitted admissioncontract.PreparedView
	f.native.CreateFunc = func(_ context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		f.creates.Add(1)
		submitted = p.View()
		return binding(submitted), nil
	}
	s := f.new(t)
	r := f.gate(t, s)
	copy(defaults, []byte(`{"spec":{"templateDefaults":{"container":{"image":"oberth.ci/build:1","workingDir":"/tmp/other"}}}}`))
	result, err := s.Deliver(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := f.runs.Admission("r1")
	if !ok {
		t.Fatal("no acceptance")
	}
	v := a.Prepared.View()
	assertSealedHandoffs(t, f, submitted, v)
	if result.Outcomes[0].State != "started" || f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("repeated preparation or submission")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal([]byte(a.Actions[0].Details), &details); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := json.Unmarshal(details["spec_digest"], &digest); err != nil {
		t.Fatal(err)
	}
	if digest != v.SpecDigest || string(details["sources"]) == "null" {
		t.Fatal("audit differs from the seal")
	}
}
func assertSealedHandoffs(t *testing.T, f *fixture, submitted, v admissioncontract.PreparedView) {
	t.Helper()
	if !reflect.DeepEqual(submitted, v) {
		t.Fatal("Create changed the seal")
	}
	if strings.Contains(string(submitted.Defaults.WorkflowDefaults), "other") {
		t.Fatal("live defaults changed submission")
	}
	if !reflect.DeepEqual(f.keeper.Bindings("r1"), v.SourceBindings) {
		t.Fatal("Keep changed bindings")
	}
	submitted.Workflow.Spec.Entrypoint = "changed"
	a, _ := f.runs.Admission("r1")
	if a.Prepared.View().Workflow.Spec.Entrypoint != "build" {
		t.Fatal("mutable seal")
	}
}
func TestChangedTemplateAfterSealCannotChangeSubmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Entrypoint = "entry"
	w.Spec.Templates = []wfv1.Template{{Name: "entry", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", TemplateRef: &wfv1.TemplateRef{Name: "shared", Template: "build"}}}}}}
	f.setDocument(w)
	shared := &wfv1.WorkflowTemplate{Spec: workflow().Spec}
	shared.Name = "shared"
	shared.Namespace = "runs"
	shared.UID = "template-uid"
	shared.ResourceVersion = "1"
	load := f.authority.LoadFunc
	reads := 0
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		q, p, err := load(ctx, scope)
		p.TemplateGetter = func(context.Context, string) (*wfv1.WorkflowTemplate, error) { reads++; return shared.DeepCopy(), nil }
		return q, p, err
	}
	s := f.new(t)
	r := f.gate(t, s)
	before := reads
	shared.Spec.Templates[0].Container.Image = "changed/image"
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if reads != before || f.prepares.Load() != 1 {
		t.Fatal("template was reloaded after sealing")
	}
}
func TestCheckHasNoEffects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p, err := f.new(t).Check(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if p.View().RunID != "" || f.creates.Load() != 0 {
		t.Fatal("inspection created work")
	}
	if _, ok := f.runs.Admission("r1"); ok {
		t.Fatal("inspection saved a run")
	}
}
func TestReleaseSelectedByReleaseClaimOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, kind, ref, file string
		trigger               model.RunTrigger
		release               bool
	}{
		{"branch", "release", "refs/heads/topic", workflowpolicy.BuildFile, model.TriggerBranch, false},
		{"tag_trigger_on_head", "release", "refs/heads/topic", workflowpolicy.BuildFile, model.TriggerTag, false},
		{"ordinary", "ordinary", "refs/tags/demo", workflowpolicy.BuildFile, model.TriggerTag, false},
		{"version", "release", "refs/tags/v1.0.0", workflowpolicy.ReleaseFile, model.TriggerTag, true},
		{"route", "release", "refs/tags/v2.0.0", ".oberth/publish.yaml", model.TriggerTag, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := request()
			r.Ref.Name = tc.ref
			r.Release = tc.kind == "release"
			r.Trigger = tc.trigger
			f.d.Routes = workflowpolicy.ReleaseRoutes{r.Repository: {{Pattern: "v2.*", File: ".oberth/publish.yaml"}}}
			load := f.authority.LoadFunc
			f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
				if scope.File != tc.file || scope.Release != tc.release {
					t.Errorf("wrong scope %+v", scope)
				}
				return load(ctx, scope)
			}
			f.d.Resolver = fake.StartResolver{Resolve: func(context.Context, service.StartRequest) (app.Request, error) { return r, nil }}
			s := f.new(t)
			if _, err := s.StartRun(context.Background(), startRequest(tc.name, r)); err != nil {
				t.Fatal(err)
			}
			a, _ := f.runs.Admission("r1")
			tier := "ci"
			if tc.release {
				tier = "release"
			}
			if a.Release != tc.release || a.Actions[0].Action != tier+".argo.submit.binding" {
				t.Fatal("audit tier differs from admitted scope", a.Actions[0].Action)
			}
		})
	}
}
func TestAdmissionRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, code string
		change     func(*fixture, *app.Request)
	}{
		{"actor", "request", func(_ *fixture, r *app.Request) { r.Actor.Identity = "" }},
		{"scope", "request", func(_ *fixture, r *app.Request) { r.Actor.Scope = []string{"other"} }},
		{"run", "request", func(_ *fixture, r *app.Request) { r.RunID = "" }},
		{"repoid", "request", func(_ *fixture, r *app.Request) { r.RepositoryID = 0 }},
		{"repo", "request", func(_ *fixture, r *app.Request) { r.Repository = "widget" }},
		{"trigger", "request", func(_ *fixture, r *app.Request) { r.Trigger = "" }},
		{"missing_file", "missing", func(f *fixture, _ *app.Request) { f.source.Read = missingFile }},
		{"missing_commit", "read", func(f *fixture, _ *app.Request) {
			f.source.Read = func(context.Context, gitport.Revision, string) (gitport.File, error) {
				return gitport.File{}, &gitport.AbsentError{Ref: "commit"}
			}
		}},
		{"read", "read", func(f *fixture, _ *app.Request) {
			f.source.Read = func(context.Context, gitport.Revision, string) (gitport.File, error) {
				return gitport.File{}, errors.New("read unavailable")
			}
		}},
		{"authority", "unavailable", func(f *fixture, _ *app.Request) {
			f.authority.LoadFunc = func(context.Context, app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
				return admissioncontract.PrepareRequest{}, argoworkflow.Policy{}, errors.New("authority offline")
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := request()
			tc.change(f, &r)
			_, err := f.new(t).Admit(context.Background(), r, f.source)
			expectRefusal(t, err, tc.code)
		})
	}
}
func TestRealAdmissionFindingsAndGrantCommandReachPusher(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "kv/build"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	f.setDocument(w)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	_, err := s.Gate(context.Background(), r)
	var authored *argoworkflow.AdmissionError
	if !errors.As(err, &authored) {
		t.Fatalf("not the real admission error: %v", err)
	}
	text := err.Error()
	command := "oberth secret put --repo 'origin/team/widget' --file '.oberth/build.yaml' --step 'build' 'kv/build' < <value-file>"
	if !strings.Contains(text, "\n  fix: Administrator: "+command) || !strings.Contains(text, "secrets[kv/build]") || strings.Contains(text, "detail: "+command) {
		t.Fatal(text)
	}
	if strings.Contains(text, "prepare:") || strings.Contains(text, "pipeline admission failed") {
		t.Fatal("internal/repeated header", text)
	}
}
func TestAuditLabelsGrantInventoryHonestly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		q, p, err := load(ctx, scope)
		q.Grants = []admissioncontract.GrantEvidence{{Repository: scope.Repository, File: scope.File, Step: "unused", Kind: "secret", Path: "kv/unused", Revision: "1", State: admissioncontract.StateVerified}}
		return q, p, err
	}
	s := f.new(t)
	if _, err := s.StartRun(context.Background(), startRequest("inventory", request())); err != nil {
		t.Fatal(err)
	}
	a, _ := f.runs.Admission("r1")
	var details map[string]json.RawMessage
	if err := json.Unmarshal([]byte(a.Actions[0].Details), &details); err != nil {
		t.Fatal(err)
	}
	if details["declared_secret_paths"] != nil || details["step_credentials"] != nil {
		t.Fatal("grant inventory called declarations")
	}
	if details["granted_secret_paths"] != nil || !strings.Contains(string(details["granted_step_credentials"]), "kv/unused") {
		t.Fatal(details)
	}
}

func TestEachGrantCommandIsTheFixOfItsOwnFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "kv/one,kv/two"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	f.setDocument(w)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	_, err := s.Gate(t.Context(), r)
	if err == nil {
		t.Fatal("missing grants accepted")
	}
	lines := strings.Split(err.Error(), "\n")
	for _, path := range []string{"kv/one", "kv/two"} {
		command := "  fix: Administrator: oberth secret put --repo 'origin/team/widget' --file '.oberth/build.yaml' --step 'build' '" + path + "' < <value-file>"
		found := false
		for i, line := range lines {
			if strings.Contains(line, "secrets["+path+"]") {
				found = i+1 < len(lines) && lines[i+1] == command
				break
			}
		}
		if !found {
			t.Fatalf("grant command is not beside its finding: %s\n%s", path, err)
		}
	}
}
