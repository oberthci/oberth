package argoworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
)

func TestPreparedOnceTransformThenNativeSchemaPlanAndSeal(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Status.StoredWorkflowSpec = &wfv1.WorkflowSpec{ServiceAccountName: "authored"}
	r := requestFor(t, wf)
	before := r.Clone()
	hooks := identityHooks()
	transforms, identities := 0, 0
	verify := hooks.VerifyIdentities
	hooks.VerifyIdentities = func(ctx context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
		identities++
		return verify(ctx, r)
	}
	var retained *wfv1.Workflow
	hooks.TransformNative = func(_ context.Context, w *wfv1.Workflow, input admissioncontract.TransformInput) error {
		transforms++
		retained = w
		assertBoundIdentity(t, w)
		w.Name = "run-1"
		w.Spec.Templates[0].Container.Args = []string{"transformed"}
		input.Identity.Pipeline.UID = "changed"
		return nil
	}
	prepared, err := prepareFixture(t.Context(), r, submitPolicy(), hooks)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if transforms != 1 || identities != 1 {
		t.Fatal(transforms, identities)
	}
	view := prepared.View()
	if len(view.Plan.Entries) != 1 || !view.Plan.Exact || view.Workflow.Status.StoredWorkflowSpec != nil || view.Identity.Pipeline.UID == "changed" {
		t.Fatalf("%+v", view)
	}
	frozen, err := prepared.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	retained.Spec.Templates[0].Container.Args[0] = "changed"
	view.Workflow.Spec.Templates[0].Container.Image = "changed"
	view.Plan.Entries[0].Name = "changed"
	again, err := prepared.MarshalJSON()
	if err != nil || !bytes.Equal(frozen, again) {
		t.Fatal("sealed data changed", err)
	}
	after, _ := json.Marshal(r)
	original, _ := json.Marshal(before)
	if !bytes.Equal(after, original) {
		t.Fatal("request mutated")
	}
	assertPreparedDigest(t, prepared)

}
func TestPreparationRefusesBeforeIdentityAndTransformation(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.PodSpecPatch = "{}"
	r := requestFor(t, wf)
	hooks := admissioncontract.PrepareHooks{VerifyIdentities: func(context.Context, admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
		t.Fatal("identity called")
		return admissioncontract.IdentityEvidence{}, nil
	}, TransformNative: func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
		t.Fatal("transform called")
		return nil
	}}
	result, err := prepareFixture(t.Context(), r, submitPolicy(), hooks)
	if result != nil {
		t.Fatal("sealed refusal")
	}
	requireCode(t, err, "patch")
}
func TestPreparationBindsExactReceiptAndIdentity(t *testing.T) {
	t.Parallel()
	base := requestFor(t, simpleWorkflow())
	for _, field := range []string{"repository", "object", "commit", "file", "blob", "document", "account", "mode", "trigger", "schedule"} {
		t.Run(field, func(t *testing.T) {
			r := base.Clone()
			changeRequest(&r, field)

			if got, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks()); err == nil || got != nil {
				t.Fatalf("accepted %s", field)
			}
		})
	}
}
func TestIdentityUnknownNeverAuthorizesSubmission(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	if result, err := prepareFixture(t.Context(), r, submitPolicy(), admissioncontract.PrepareHooks{}); result != nil || err == nil {
		t.Fatal("unknown identity authorized")
	}
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	prepared, err := prepareFixture(t.Context(), r, submitPolicy(), admissioncontract.PrepareHooks{})
	if err != nil || prepared.View().Identity.State != admissioncontract.StateLiveRequired {
		t.Fatal(prepared, err)
	}
}
func TestInspectMissingSharedTemplateIsExplicitDynamicEvidence(t *testing.T) {
	t.Parallel()
	r := requestFor(t, sharedConsumer("shared", "work"))
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	result, err := prepareFixture(t.Context(), r, submitPolicy(), admissioncontract.PrepareHooks{})
	if err != nil {
		t.Fatal(err)
	}
	v := result.View()
	if v.Plan.Exact || len(v.Findings) == 0 || v.Findings[0].Code != "live" {
		t.Fatalf("%+v", v)
	}
	r.Mode = admissioncontract.ModeSubmit
	r.IdentityRequest.Mode = r.Mode
	if result, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks()); result != nil || err == nil {
		t.Fatal("deferred submit")
	}
}
func TestDefaultsAndSharedTemplatesRemainCopiedAndDigested(t *testing.T) {
	t.Parallel()
	base := simpleWorkflow()
	raw, err := json.Marshal(&wfv1.Workflow{Spec: wfv1.WorkflowSpec{TemplateDefaults: &wfv1.Template{Container: &corev1.Container{WorkingDir: "/custom"}}}})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := sourcecontract.CanonicalJSON(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	r := requestFor(t, base)
	r.Defaults = runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: raw, Digest: sourcecontract.Digest(canonical)}
	first, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if first.View().Workflow.Spec.TemplateDefaults.Container.WorkingDir != "/custom" {
		t.Fatal("defaults lost")
	}
	r.Defaults.Digest = "changed"
	requireCode(t, prepareError(t, r), "binding")
	obj := sharedObject("shared")
	r = requestFor(t, sharedConsumer("shared", "work"))
	p := submitPolicy()
	calls := 0
	p.TemplateGetter = func(context.Context, string) (*wfv1.WorkflowTemplate, error) { calls++; return obj, nil }
	result, err := prepareFixture(t.Context(), r, p, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result.View().Templates) != 1 {
		t.Fatalf("reads=%d", calls)
	}
	before := result.View().Digest
	obj.Spec.Templates[0].Container.Image = "changed"
	other, err := prepareFixture(t.Context(), r, p, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if before == other.View().Digest {
		t.Fatal("snapshot mutation not bound")
	}
}
func prepareError(t *testing.T, r admissioncontract.PrepareRequest) error {
	t.Helper()
	_, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	return err
}
func TestSourcePreparedBindingAndFinalSubmittedSize(t *testing.T) {
	t.Parallel()
	wf := sourceWorkflow()
	r := requestFor(t, wf)
	bindings := []sourcecontract.Binding{{Template: "main", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: r.SourceReceipt.Revision}}
	result, err := prepareFixture(t.Context(), r, submitPolicy(), sourceHooks(bindings...))
	if err != nil {
		t.Fatal(err)
	}
	if result.View().Workflow.Spec.Arguments.GetParameterByName("oberth-source") == nil {
		t.Fatal("source parameter absent")
	}
	bindings[0].Revision.Commit = strings.Repeat("d", 40)
	_, err = Prepare(t.Context(), r, Policy{}, sourceHooks(bindings...))
	requireCode(t, err, "binding")
	bindings[0].Revision = r.SourceReceipt.Revision
	bindings[0].Export.Ref = sourcecontract.Ref{Name: "refs/heads/other", ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA}
	_, err = Prepare(t.Context(), r, Policy{}, sourceHooks(bindings...))
	requireCode(t, err, "binding")
	bindings[0].Export.Ref = sourcecontract.Ref{}
	hooks := sourceHooks(bindings...)
	hooks.TransformNative = func(_ context.Context, w *wfv1.Workflow, _ admissioncontract.TransformInput) error {
		w.Spec.Templates[0].Container.Args = []string{strings.Repeat("x", admissioncontract.MaxSourceSpecBytes)}
		return nil
	}
	_, err = prepareFixture(t.Context(), r, submitPolicy(), hooks)
	requireCode(t, err, "source_size")
}
func TestFinalNativeSchemaRejectsMalformedTransform(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	hooks := identityHooks()
	hooks.TransformNative = func(_ context.Context, w *wfv1.Workflow, _ admissioncontract.TransformInput) error {
		w.Spec.Templates[0].Container.Image = ""
		return nil
	}
	result, err := prepareFixture(t.Context(), r, submitPolicy(), hooks)
	if result != nil {
		t.Fatal("sealed invalid native spec")
	}
	requireCode(t, err, "native")
}
func TestConcurrentPreparationUsesPrivateNativeLock(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	p := submitPolicy()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := prepareFixture(t.Context(), r, p, identityHooks()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
func TestScheduleIdentityIsExactAndNeverGuessed(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	r.Trigger = model.TriggerSchedule
	r.ScheduleEntry = "nightly"
	prepared, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.View().ScheduleEntry != "nightly" {
		t.Fatal("schedule changed")
	}
	for _, entry := range []string{"", strings.Repeat("a", 65), "{{dynamic}}"} {
		r.ScheduleEntry = entry
		requireCode(t, prepareError(t, r), "schedule")
	}
}

func assertPreparedDigest(t *testing.T, prepared *admissioncontract.PreparedAdmission) {
	t.Helper()
	canonical, err := sourcecontract.CanonicalJSON(prepared.View().Workflow.Spec)
	if err != nil || sourcecontract.Digest(canonical) != prepared.View().SpecDigest {
		t.Fatal("Create and digest differ", err)
	}
}

func changeRequest(r *admissioncontract.PrepareRequest, field string) {
	switch field {
	case "repository":
		r.Repository = "forge/other/repo"
	case "object":
		r.ObjectSHA = strings.Repeat("c", 40)
	case "commit":
		r.CommitSHA = strings.Repeat("c", 40)
	case "file":
		r.SelectedFile = ".oberth/other.yaml"
	case "blob":
		r.FileBlob = strings.Repeat("c", 40)
	case "document":
		r.Document = append(r.Document, ' ')
	case "account":
		r.IdentityRequest.PipelineAccount = "default"
	case "mode":
		r.Mode = ""
	case "trigger":
		r.Trigger = ""
	case "schedule":
		r.ScheduleEntry = "nightly"
	}
}

func assertBoundIdentity(t *testing.T, w *wfv1.Workflow) {
	t.Helper()
	if w.Namespace != "ci" || w.Spec.ServiceAccountName != "ci-step" || w.Spec.AutomountServiceAccountToken == nil || *w.Spec.AutomountServiceAccountToken {
		t.Fatal("unbound transformation")
	}
}

func TestInspectCanReportUnknownIdentityWithoutInventingAccounts(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest = admissioncontract.IdentityRequest{}
	result, err := prepareFixture(t.Context(), r, admissionFixture{}, admissioncontract.PrepareHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if result.View().Identity.State != admissioncontract.StateLiveRequired || len(result.View().Findings) == 0 {
		t.Fatal("unknown identity hidden")
	}
}
func TestForwardedSourceBindsTheConcreteDestination(t *testing.T) {
	t.Parallel()
	wf := sourceWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	work.Inputs.Artifacts[0].Plugin = nil
	source := mainSource()
	source.Path = ""
	source.Mode = nil
	wf.Spec.Templates = []wfv1.Template{{Name: "main", Inputs: wfv1.Inputs{Artifacts: []wfv1.Artifact{source}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "work", Arguments: wfv1.Arguments{Artifacts: []wfv1.Artifact{{Name: "source", From: "{{inputs.artifacts.source}}"}}}}}}}, work}
	r := requestFor(t, wf)
	bindings := []sourcecontract.Binding{{Template: "work", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Revision: r.SourceReceipt.Revision, RepositoryID: 1}}
	if _, err := prepareFixture(t.Context(), r, submitPolicy(), sourceHooks(bindings...)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectGrantFindingsDoNotHideStructuralRefusals(t *testing.T) {
	t.Parallel()
	wf, _ := tokenWorkflow()
	r := requestFor(t, wf)
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	p := submitPolicy()
	p.LoginAudience = "login"
	result, err := prepareFixture(t.Context(), r, p, admissioncontract.PrepareHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.View().Findings) < 2 {
		t.Fatal("grant or live identity findings lost")
	}
	wf.Spec.PodSpecPatch = "{}"
	r.Document = mustDocument(t, wf)
	_, err = prepareFixture(t.Context(), r, p, admissioncontract.PrepareHooks{})
	requireCode(t, err, "patch")
}
func TestRetainedSnapshotNeedsLiveReadForSubmission(t *testing.T) {
	t.Parallel()
	obj := sharedObject("shared")
	raw, err := json.Marshal(obj.Spec)
	if err != nil {
		t.Fatal(err)
	}
	r := requestFor(t, sharedConsumer("shared", "work"))
	r.Templates = []admissioncontract.TemplateSnapshot{{Namespace: "ci", Name: "shared", UID: "uid", ResourceVersion: "1", Spec: raw}}
	requireCode(t, prepareError(t, r), "live")
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	result, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if !result.View().Plan.Exact || len(result.View().Templates) != 1 || !hasLiveFinding(result.View().Findings) {
		t.Fatal("snapshot/live distinction lost")
	}
}
func mustDocument(t *testing.T, wf *wfv1.Workflow) []byte {
	t.Helper()
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func hasLiveFinding(findings []admissioncontract.Finding) bool {
	for _, f := range findings {
		if f.Code == "live" {
			return true
		}
	}
	return false
}

func TestSourceMutationInTransformCannotDivergeFromFrozenBindings(t *testing.T) {
	t.Parallel()
	r := requestFor(t, sourceWorkflow())
	bindings := []sourcecontract.Binding{{Template: "main", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: r.SourceReceipt.Revision}}
	hooks := sourceHooks(bindings...)
	hooks.TransformNative = func(_ context.Context, w *wfv1.Workflow, _ admissioncontract.TransformInput) error {
		w.Spec.Templates[0].Inputs.Artifacts[0].Path = "/other"
		return nil
	}
	result, err := prepareFixture(t.Context(), r, submitPolicy(), hooks)
	if result != nil {
		t.Fatal("sealed changed source")
	}
	requireCode(t, err, "binding")
}
func TestInspectStillChecksKnownNativeSchema(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.Templates[0].Suspend = &wfv1.SuspendTemplate{}
	r := requestFor(t, wf)
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	result, err := prepareFixture(t.Context(), r, submitPolicy(), admissioncontract.PrepareHooks{})
	if result != nil {
		t.Fatal("invalid native schema accepted during inspection")
	}
	requireCode(t, err, "native")
}

func TestDefaultsDigestBindsTheCanonicalSnapshot(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	raw := json.RawMessage(`{"spec":{"templateDefaults":{"container":{"workingDir":"/custom"}}}}`)
	canonical, err := sourcecontract.CanonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Defaults = runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: raw, Digest: sourcecontract.Digest(canonical)}
	result, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.View().Defaults.WorkflowDefaults, raw) {
		t.Fatal("snapshot changed")
	}
}

func TestInspectSourceBindingsAreExplicitlyLiveRequired(t *testing.T) {
	t.Parallel()
	r := requestFor(t, sourceWorkflow())
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	result, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range result.View().Findings {
		if f.Path == "source_bindings" && f.Code == "live" {
			return
		}
	}
	t.Fatal("unknown source authority was hidden")
}
