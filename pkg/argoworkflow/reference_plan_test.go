package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sharedConsumer(object, template string) *wfv1.Workflow {
	wf := simpleWorkflow()
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "work", TemplateRef: &wfv1.TemplateRef{Name: object, Template: template}}}}}
	return wf
}
func sharedObject(name string) *wfv1.WorkflowTemplate {
	return &wfv1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ci", UID: "uid", ResourceVersion: "1"}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "work", Container: &corev1.Container{Image: "image"}}}}}
}
func getterFor(objects map[string]*wfv1.WorkflowTemplate) TemplateGetter {
	return func(_ context.Context, name string) (*wfv1.WorkflowTemplate, error) {
		return objects[name].DeepCopy(), nil
	}
}

// Covers: REQ-C4-004
func TestFiniteCanonicalPreparationContract(t *testing.T) {
	t.Parallel()
	preparationReadCases(t)
	wf := sharedConsumer("shared", "work")
	obj := sharedObject("shared")
	obj.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "again", Template: "work"}, {Name: "leaf", Template: "leaf"}}}}
	obj.Spec.Templates = append(obj.Spec.Templates, wfv1.Template{Name: "leaf", Container: &corev1.Container{Image: "image"}})
	p := admissionFixture{Policy: Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj})}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	plan, err := planFixture(t.Context(), wf, p)
	if err != nil || plan.Exact || len(plan.Entries) != 2 || !plan.Entries[0].Dynamic {
		t.Fatalf("local recursion: %+v %v", plan, err)
	}
	// A completed local component has a later external edge back to itself.
	obj.Spec.Templates[0].DAG.Tasks = append(obj.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "cross", TemplateRef: &wfv1.TemplateRef{Name: "second", Template: "work"}})
	second := sharedObject("second")
	second.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "back", TemplateRef: &wfv1.TemplateRef{Name: "shared", Template: "work"}}}}}
	p.TemplateGetter = getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj, "second": second})
	requireCode(t, admitFixture(t.Context(), wf, p), "cycle")
}

// Covers: REQ-C4-007
func TestHelpfulPreparationFailuresContract(t *testing.T) {
	t.Parallel()
	wf := sharedConsumer("shared", "work")
	failure := errors.New("reader unavailable")
	calls := 0
	p := admissionFixture{Policy: Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) {
		calls++
		if calls == 1 {
			return nil, failure
		}
		return sharedObject("shared"), nil
	}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}},
	}
	var read *TemplateReadError
	if err := admitFixture(t.Context(), wf, p); !errors.As(err, &read) || !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal("failed read cached", err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	p.TemplateGetter = func(context.Context, string) (*wfv1.WorkflowTemplate, error) { return nil, nil }
	requireCode(t, admitFixture(t.Context(), wf, p), "template_missing")
}
func TestSharedReadsOnceAndNoConsumerGrantInheritance(t *testing.T) {
	t.Parallel()
	wf := sharedConsumer("shared", "work")
	wf.Spec.Templates[0].DAG.Tasks = append(wf.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "second", TemplateRef: &wfv1.TemplateRef{Name: "shared", Template: "work"}})
	obj := sharedObject("shared")
	calls := 0
	p := admissionFixture{Policy: Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) { calls++; return obj, nil }}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
	if err := admitFixture(t.Context(), wf, p); err != nil || calls != 1 {
		t.Fatalf("%v reads=%d", err, calls)
	}
	obj.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "approved"}}}}
	p.PVCClaims = map[string]bool{"approved": true}
	requireCode(t, admitFixture(t.Context(), wf, p), "pvc")
	obj.Spec.Templates[0].Volumes = nil
	obj.Spec.Templates[0].ServiceAccountName = "other"
	requireCode(t, admitFixture(t.Context(), wf, p), "identity")
}
func TestReferencesDepthAndGetterIdentity(t *testing.T) {
	t.Parallel()
	wf := sharedConsumer("r0", "work")
	objects := map[string]*wfv1.WorkflowTemplate{}
	for i := range 65 {
		obj := sharedObject(fmt.Sprintf("r%d", i))
		if i < 64 {
			obj.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "next", TemplateRef: &wfv1.TemplateRef{Name: fmt.Sprintf("r%d", i+1), Template: "work"}}}}}
		}
		objects[obj.Name] = obj
	}
	p := admissionFixture{Policy: Policy{TemplateGetter: getterFor(objects)}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
	requireCode(t, admitFixture(t.Context(), wf, p), "depth")
	p.TemplateGetter = func(context.Context, string) (*wfv1.WorkflowTemplate, error) { return sharedObject("different"), nil }
	requireCode(t, admitFixture(t.Context(), wf, p), "reference")
	wf.Spec.Templates[0].DAG.Tasks[0].TemplateRef.ClusterScope = true
	requireCode(t, admitFixture(t.Context(), wf, p), "reference")
}
func TestCompleteLogicalPlanDeclarationOrder(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.Templates = []wfv1.Template{
		{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "first", Template: "one", Hooks: wfv1.LifecycleHooks{"exit": {Template: "two"}}}, {Name: "second", Inline: &wfv1.Template{Suspend: &wfv1.SuspendTemplate{}}}}}, {Steps: []wfv1.WorkflowStep{{Name: "third", Template: "set"}}}}},
		{Name: "one", Container: &corev1.Container{Image: "image"}},
		{Name: "two", Resource: &wfv1.ResourceTemplate{}},
		{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "a", Image: "image"}}, {Container: corev1.Container{Name: "b", Image: "image"}}}}},
	}
	wf.Spec.OnExit = "two"
	wf.Spec.Hooks = wfv1.LifecycleHooks{"running": {Template: "one"}}
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[0].first", "[0].first.onExit", "[0].second", "[1].third", "onExit", "hooks.running"}
	var paths []string
	for _, entry := range plan.Entries {
		paths = append(paths, entry.Path)
	}
	if !plan.Exact || !reflect.DeepEqual(paths, want) {
		t.Fatalf("%+v", plan)
	}
	root, err := planFixture(t.Context(), simpleWorkflow(), admissionFixture{})
	if err != nil || len(root.Entries) != 1 || root.Entries[0].Path != "" {
		t.Fatalf("root: %+v %v", root, err)
	}
}
func TestLiteralLoopsNativePathsAndDynamicPlaceholder(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	leaf := wf.Spec.Templates[0]
	leaf.Name = "leaf"
	wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "literal", Template: "leaf", WithItems: []wfv1.Item{{Value: json.RawMessage(`"hello(world)"`)}, {Value: json.RawMessage(`{"b":2,"a":1}`)}}}, {Name: "runtime", Template: "leaf", WithParam: "{{workflow.parameters.items}}"}}}}, leaf}
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"literal(0:helloworld)", "literal(1:a:1,b:2)", "runtime"}
	var got []string
	for _, e := range plan.Entries {
		got = append(got, e.Path)
	}
	if !reflect.DeepEqual(got, want) || plan.Exact || !plan.Entries[2].Dynamic {
		t.Fatalf("%+v", plan)
	}
}
func TestPlanExpansionBoundsCollapseWithoutRefusal(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"entries", "bytes", "visits"} {
		t.Run(which, func(t *testing.T) {
			wf := expansionWorkflow(which)

			plan, err := planFixture(t.Context(), wf, admissionFixture{})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Exact || len(plan.Entries) == 0 || len(plan.Entries) > admissioncontract.MaxPlanEntries || len(raw) > admissioncontract.MaxPlanBytes {
				t.Fatalf("entries=%d bytes=%d exact=%v", len(plan.Entries), len(raw), plan.Exact)
			}
		})
	}
}

func expansionWorkflow(which string) *wfv1.Workflow {
	wf := simpleWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	loop := wfv1.DAGTask{Name: "loop", Template: "work"}
	switch which {
	case "entries":
		for range admissioncontract.MaxPlanEntries + 1 {
			loop.WithItems = append(loop.WithItems, wfv1.Item{Value: json.RawMessage(`1`)})
		}
	case "bytes":
		for range 32 {
			loop.WithItems = append(loop.WithItems, wfv1.Item{Value: json.RawMessage(`"` + strings.Repeat("a", 40000) + `"`)})
		}
	case "visits":
		work = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{}}
		for i := range 5 {
			work.DAG.Tasks = append(work.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("empty%d", i), Template: "empty"})
		}
		for range 14000 {
			loop.WithItems = append(loop.WithItems, wfv1.Item{Value: json.RawMessage(`1`)})
		}
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "empty", DAG: &wfv1.DAGTemplate{}})
	}
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{loop}}}
	wf.Spec.Templates = append(wf.Spec.Templates, work)
	return wf
}

func TestLoopHooksUseEachNativeInvocationPath(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "loop", Template: "work", WithItems: []wfv1.Item{{Value: json.RawMessage(`"a"`)}, {Value: json.RawMessage(`"b"`)}}, Hooks: wfv1.LifecycleHooks{"exit": {Template: "work"}}}}}}, work}
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"loop(0:a)", "loop(0:a).onExit", "loop(1:b)", "loop(1:b).onExit", "loop.onExit"}
	var got []string
	for _, e := range plan.Entries {
		got = append(got, e.Path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	wf.Spec.Templates[0].DAG.Tasks[0].WithItems[0].Value = json.RawMessage(`"{{workflow.parameters.item}}"`)
	plan, err = planFixture(t.Context(), wf, admissionFixture{})
	if err != nil || plan.Exact || len(plan.Entries) != 1 {
		t.Fatal(plan, err)
	}
}
func TestDynamicLocalCallsKeepAllPotentialSourceUsage(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	work.Container.WorkingDir = "/work/src"
	wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "choose", Template: "{{inputs.parameters.target}}"}}}}, work}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "source_usage")
	wf.Spec.Templates[1].Container.WorkingDir = ""
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil || plan.Exact || len(plan.Entries) != 1 || !plan.Entries[0].Dynamic {
		t.Fatal(plan, err)
	}
}

func TestPlanRootCollapseHasNameAndConservativeCredentialIntent(t *testing.T) {
	t.Parallel()
	wf := expansionWorkflow("entries")
	wf.Spec.Templates[0].DAG.Tasks[0].WithItems = wf.Spec.Templates[0].DAG.Tasks[0].WithItems[:admissioncontract.MaxPlanEntries]
	wf.Spec.OnExit = "work"
	wf.Spec.Templates[1].Container.Command = []string{"oberth", "secretstore", "exec"}
	declareSecretPaths(&wf.Spec.Templates[1], "public/key")
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Exact || len(plan.Entries) != 1 {
		t.Fatalf("%+v", plan)
	}
	root := plan.Entries[0]
	if root.Path != "" || root.Name != "main" || !root.Dynamic || !root.CredentialIntent {
		t.Fatalf("%+v", root)
	}
}
func TestPlanExactJSONByteBoundary(t *testing.T) {
	t.Parallel()
	entry := admissioncontract.PlanEntry{Path: "work", Name: "", Template: "private"}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{entry}, Exact: true}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	entry.Name = strings.Repeat("a", admissioncontract.MaxPlanBytes-len(raw))
	p := planner{plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{}, Exact: true}, emptyBytes: emptyPlanBytes()}
	if !p.append(entry) {
		t.Fatal("exact byte-boundary entry collapsed")
	}
	raw, err = json.Marshal(p.plan)
	if err != nil || len(raw) != admissioncontract.MaxPlanBytes {
		t.Fatal(len(raw), err)
	}
	entry.Name += "a"
	p.plan.Entries = nil
	p.bytes = 0
	if p.append(entry) {
		t.Fatal("over-limit encoded plan accepted")
	}
}
