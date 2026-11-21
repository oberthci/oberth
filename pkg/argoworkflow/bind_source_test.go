package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"reflect"
	"strings"
	"testing"
)

func pinInput(t *testing.T, name string) wfv1.Artifact {
	t.Helper()
	a := mainSource()
	a.Name = name
	a.Path = "/work/sources/" + name
	raw, err := json.Marshal(sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "forge/org/library", Commit: strings.Repeat("c", 40)})
	if err != nil {
		t.Fatal(err)
	}
	a.Plugin.Configuration = string(raw)
	return a
}
func boundEvidence(r admissioncontract.PrepareRequest, d sourcecontract.Descriptor) sourcecontract.Binding {
	b := sourcecontract.Binding{Descriptor: d, RepositoryID: 1, Revision: r.SourceReceipt.Revision}
	if d.Kind == sourcecontract.Pinned {
		b.RepositoryID = 2
		b.Revision = sourcecontract.Revision{Repository: d.Repository, Commit: d.Commit}
		b.AuthorizationDigest = sourcecontract.Digest([]byte("approved source"))
	}
	return b
}
func TestPrepareBindsDeclaredSourcesOnceBeforeNativeTransform(t *testing.T) {
	t.Parallel()
	w := sourceWorkflow()
	w.Spec.Templates[0].Inputs.Artifacts = append(w.Spec.Templates[0].Inputs.Artifacts, pinInput(t, "library"), pinInput(t, "vendor"))
	r := requestFor(t, w)
	hooks := identityHooks()
	var seen []sourcecontract.Descriptor
	retained := []sourcecontract.Ref{{Name: "refs/tags/retained", ObjectSHA: r.ObjectSHA}}
	hooks.BindSource = func(ctx context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		if ctx != t.Context() {
			t.Fatal("source binder lost context")
		}
		seen = append(seen, d)
		b := boundEvidence(r, d)
		// The provider need not know YAML coordinates; C4 owns them.
		b.Template, b.Artifact, b.Path = "provider-template", "provider-artifact", "/provider"
		if d.Kind == sourcecontract.Main {
			b.Export.Tags = retained
		}
		return b, nil
	}
	hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, input admissioncontract.TransformInput) error {
		if len(seen) != 3 || len(input.SourceBindings) != 3 {
			t.Fatal("source binding must finish before native transformation")
		}
		retained[0].ObjectSHA = strings.Repeat("d", 40)
		input.SourceBindings[0].Export.Tags[0].ObjectSHA = strings.Repeat("e", 40)
		return nil
	}
	prepared, err := Prepare(t.Context(), r, Policy{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	assertBoundSourceSnapshots(t, prepared, seen, r.ObjectSHA)
}

func TestBindSourceDiscoversLocalSharedAndInlineDeclarations(t *testing.T) {
	t.Parallel()
	for _, carrier := range []string{"local", "shared", "inline"} {
		t.Run(carrier, func(t *testing.T) {
			w := simpleWorkflow()
			leaf := w.Spec.Templates[0]
			leaf.Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library")}
			p := Policy{}
			switch carrier {
			case "local":
				w.Spec.Templates[0] = leaf
			case "shared":
				obj := sharedObject("shared")
				leaf.Name = "work"
				obj.Spec.Templates = []wfv1.Template{leaf}
				w = sharedConsumer("shared", "work")
				p.TemplateGetter = getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj})
			case "inline":
				w.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "build", Inline: &leaf}}}}
			}
			r := requestFor(t, w)
			hooks := identityHooks()
			calls := 0
			hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
				calls++
				return boundEvidence(r, d), nil
			}
			prepared, err := Prepare(t.Context(), r, p, hooks)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(prepared.View().SourceBindings) != 1 {
				t.Fatal("source input not bound exactly once", calls)
			}
		})
	}
}
func TestBindingHookCannotChangeDescriptorRevisionOrAuthority(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"descriptor", "revision", "repository_id", "authorization"} {
		t.Run(field, func(t *testing.T) {
			w := simpleWorkflow()
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library")}
			r := requestFor(t, w)
			hooks := identityHooks()
			hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
				b := boundEvidence(r, d)
				switch field {
				case "descriptor":
					b.Descriptor.Commit = strings.Repeat("d", 40)
					b.Revision.Commit = b.Descriptor.Commit
				case "revision":
					b.Revision.Commit = strings.Repeat("d", 40)
				case "repository_id":
					b.RepositoryID = 0
				case "authorization":
					b.AuthorizationDigest = ""
				}
				return b, nil
			}
			result, err := Prepare(t.Context(), r, Policy{}, hooks)
			if result != nil {
				t.Fatal("sealed invalid source evidence")
			}
			requireCode(t, err, "binding")
		})
	}
}
func TestBindingHookFailureNamesInputAndPreservesCause(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library")}
	cause := errors.New("producer source is not granted")
	hooks := identityHooks()
	transforms := 0
	hooks.BindSource = func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		return sourcecontract.Binding{}, cause
	}
	hooks.TransformNative = func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
		transforms++
		return nil
	}
	result, err := Prepare(t.Context(), requestFor(t, w), Policy{}, hooks)
	if result != nil || !errors.Is(err, cause) || transforms != 0 {
		t.Fatal("source refusal did not stop preparation", err)
	}
	if !strings.Contains(err.Error(), `.oberth/build.yaml: spec.templates["main"].inputs.artifacts["library"]:`) {
		t.Fatal("source refusal lost YAML path", err)
	}
}
func TestSourceBinderRunsOnlyForAdmittedDeclaredInputs(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"no source", "invalid YAML", "boundary refusal"} {
		t.Run(state, func(t *testing.T) {
			w := simpleWorkflow()
			if state == "boundary refusal" {
				w.Spec.PodSpecPatch = "{}"
				w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library")}
			}
			r := requestFor(t, w)
			if state == "invalid YAML" {
				r.Document = []byte("invalid")
			}
			hooks := identityHooks()
			calls := 0
			hooks.BindSource = func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
				calls++
				return sourcecontract.Binding{}, nil
			}
			_, err := Prepare(t.Context(), r, Policy{}, hooks)
			if (err == nil) != (state == "no source") || calls != 0 {
				t.Fatal("source binder ran before admission or without inputs", err, calls)
			}
		})
	}
}
func TestC4AssignsDistinctBindingCoordinates(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "a"}, {Name: "b", Template: "b"}}}}}
	for _, name := range []string{"a", "b"} {
		leaf := sourceWorkflow().Spec.Templates[0]
		leaf.Name = name
		w.Spec.Templates = append(w.Spec.Templates, leaf)
	}
	r := requestFor(t, w)
	hooks := identityHooks()
	calls := 0
	hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		calls++
		b := boundEvidence(r, d)
		b.Template, b.Artifact, b.Path = "same", "same", "/same"
		return b, nil
	}
	prepared, err := Prepare(t.Context(), r, Policy{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	bindings := prepared.View().SourceBindings
	if calls != 2 || !reflect.DeepEqual([]string{bindings[0].Template, bindings[1].Template}, []string{"a", "b"}) {
		t.Fatal("provider controlled native binding coordinates", bindings)
	}
}

func TestSourceBindingRefusalNamesEveryDeniedInput(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library"), pinInput(t, "vendor")}
	hooks := identityHooks()
	calls := 0
	cause := errors.New("producer source is not granted")
	hooks.BindSource = func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		calls++
		return sourcecontract.Binding{}, cause
	}
	result, err := Prepare(t.Context(), requestFor(t, w), Policy{}, hooks)
	if result != nil || calls != 2 || !errors.Is(err, cause) {
		t.Fatal("did not collect all source refusals", calls, err)
	}
	for _, name := range []string{"library", "vendor"} {
		if !strings.Contains(err.Error(), `spec.templates["main"].inputs.artifacts["`+name+`"]`) {
			t.Fatal("source offender missing", name, err)
		}
	}
}

func assertBoundSourceSnapshots(t *testing.T, prepared *admissioncontract.PreparedAdmission, seen []sourcecontract.Descriptor, object string) {
	t.Helper()
	bindings := prepared.View().SourceBindings
	if len(seen) != 3 || len(bindings) != 3 || seen[1] != seen[2] {
		t.Fatal("not once per effective input", seen)
	}
	paths := []string{"/work/src", "/work/sources/library", "/work/sources/vendor"}
	for i, name := range []string{"source", "library", "vendor"} {
		if bindings[i].Template != "main" || bindings[i].Artifact != name || bindings[i].Path != paths[i] || bindings[i].Descriptor != seen[i] {
			t.Fatal("binding differs from YAML", bindings)
		}
	}
	if bindings[0].Export.Tags[0].ObjectSHA != object {
		t.Fatal("provider or transform mutated retained source facts")
	}
}

func TestNativeTransformCannotAddAnUnboundSource(t *testing.T) {
	t.Parallel()
	w := sourceWorkflow()
	r := requestFor(t, w)
	hooks := identityHooks()
	calls := 0
	hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		calls++
		return boundEvidence(r, d), nil
	}
	hooks.TransformNative = func(_ context.Context, native *wfv1.Workflow, _ admissioncontract.TransformInput) error {
		native.Spec.Templates[0].Inputs.Artifacts = append(native.Spec.Templates[0].Inputs.Artifacts, pinInput(t, "late"))
		return nil
	}
	result, err := Prepare(t.Context(), r, Policy{}, hooks)
	if result != nil || calls != 1 {
		t.Fatal("source was rebound or sealed after transformation", calls, err)
	}
	requireCode(t, err, "binding")
}
func TestSourceBindingCancellationStopsBeforeTheNextInput(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{pinInput(t, "library"), pinInput(t, "vendor")}
	r := requestFor(t, w)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hooks := identityHooks()
	calls, transforms := 0, 0
	hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		calls++
		cancel()
		return boundEvidence(r, d), nil
	}
	hooks.TransformNative = func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
		transforms++
		return nil
	}
	result, err := Prepare(ctx, r, Policy{}, hooks)
	if result != nil || !errors.Is(err, context.Canceled) || calls != 1 || transforms != 0 {
		t.Fatal("binding continued after cancellation", calls, transforms, err)
	}
}

func TestMissingSourceBinderStopsBeforeNativeTransform(t *testing.T) {
	t.Parallel()
	hooks := identityHooks()
	transforms := 0
	hooks.TransformNative = func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
		transforms++
		return nil
	}
	result, err := Prepare(t.Context(), requestFor(t, sourceWorkflow()), Policy{}, hooks)
	if result != nil || transforms != 0 {
		t.Fatal("native transformation ran without source evidence", transforms, err)
	}
	requireCode(t, err, "binding")
}
