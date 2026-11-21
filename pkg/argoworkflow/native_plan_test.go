package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"reflect"
	"strings"
	"testing"
)

func planDAG() *wfv1.Workflow {
	w := simpleWorkflow()
	leaf := w.Spec.Templates[0]
	leaf.Name = "leaf"
	w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "leaf"}}}}, leaf}
	return w
}
func planPaths(p admissioncontract.Plan) []string {
	var paths []string
	for _, e := range p.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}
func loopItems() []wfv1.Item {
	return []wfv1.Item{{Value: json.RawMessage(`"a"`)}, {Value: json.RawMessage(`"b"`)}}
}
func TestPreparedPlanMatchesNativeDefaultAndHookSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*wfv1.Workflow)
		want   []string
	}{
		{"exit hooks", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
			w.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
		}, []string{"run", "run.onExit", "onExit"}},
		{"exit precedence", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
			w.Spec.Templates[0].DAG.Tasks[0].OnExit = "leaf"
			w.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
			w.Spec.OnExit = "leaf"
		}, []string{"run", "run.onExit", "onExit"}},
		{"dag loop lifecycle", func(w *wfv1.Workflow) {
			task := &w.Spec.Templates[0].DAG.Tasks[0]
			task.WithItems = loopItems()
			task.Hooks = wfv1.LifecycleHooks{"running": {Template: "leaf", Expression: "tasks.run.status == 'Running'"}}
		}, []string{"run(0:a)", "run(1:b)", "run.hooks.running"}},
		{"dag loop item exits", func(w *wfv1.Workflow) {
			task := &w.Spec.Templates[0].DAG.Tasks[0]
			task.WithItems = loopItems()
			task.Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
		}, []string{"run(0:a)", "run(0:a).onExit", "run(1:b)", "run(1:b).onExit", "run.onExit"}},
		{"dag loop group exit", func(w *wfv1.Workflow) {
			task := &w.Spec.Templates[0].DAG.Tasks[0]
			task.WithItems = loopItems()
			task.Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
			w.Spec.Templates[0].DAG.Tasks = append(w.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "after", Template: "leaf", Dependencies: []string{"run"}})
		}, []string{"run(0:a)", "run(0:a).onExit", "run(1:b)", "run(1:b).onExit", "run.onExit", "after"}},
		{"steps loop lifecycle", func(w *wfv1.Workflow) {
			w.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "run", Template: "leaf", WithItems: loopItems(), Hooks: wfv1.LifecycleHooks{"running": {Template: "leaf", Expression: "steps.run.status == 'Running'"}, "exit": {Template: "leaf"}}}}}}}
		}, []string{"[0].run(0:a)", "[0].run(0:a).onExit", "[0].run(0:a).hooks.running", "[0].run(1:b)", "[0].run(1:b).onExit", "[0].run(1:b).hooks.running"}},
		{"steps defaults preserve dag", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "extra", Template: "leaf"}}}}}
		}, []string{"run"}},
		{"defaults preserve dag", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Image: "example.test/default:1"}}
		}, []string{"run"}},
		{"partial defaults preserve dag", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{WorkingDir: "/other"}}
		}, []string{"run"}},
		{"recursive default reproducer", func(w *wfv1.Workflow) {
			leaf := wfv1.Template{Suspend: &wfv1.SuspendTemplate{}}
			w.Spec.TemplateDefaults = &wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &leaf}, {Name: "b", Inline: &leaf}}}}
			w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Inline: &leaf}}}}}
		}, []string{"run", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := planDAG()
			tc.change(w)
			p, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
			if err != nil {
				t.Fatal(err)
			}
			if !p.View().Plan.Exact || !reflect.DeepEqual(planPaths(p.View().Plan), tc.want) {
				t.Fatalf("got %v; want %v", planPaths(p.View().Plan), tc.want)
			}
		})
	}
}
func TestReferencedAndInlineCallsUseSubmittedWorkflowDefaults(t *testing.T) {
	t.Parallel()
	for _, inline := range []bool{false, true} {
		t.Run(fmt.Sprint(inline), func(t *testing.T) {
			w := sharedConsumer("shared", "work")
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{WorkingDir: "/work/src"}}
			obj := sharedObject("shared")
			p := Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj})}
			if inline {
				w.Spec.Templates[0].DAG.Tasks[0].TemplateRef = nil
				w.Spec.Templates[0].DAG.Tasks[0].Inline = &obj.Spec.Templates[0]
			}
			_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
			requireCode(t, err, "source_usage")
		})
	}
	w := sharedConsumer("shared", "work")
	obj := sharedObject("shared")
	obj.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "actual", Template: "leaf"}}}}
	obj.Spec.Templates = append(obj.Spec.Templates, wfv1.Template{Name: "leaf", Container: &corev1.Container{Image: "image"}})
	obj.Spec.TemplateDefaults = &wfv1.Template{Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "extra", Template: "leaf"}}}}}
	prepared, err := Prepare(t.Context(), requestFor(t, w), Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj})}, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if got := planPaths(prepared.View().Plan); !reflect.DeepEqual(got, []string{"work.actual"}) {
		t.Fatal(got)
	}
}
func TestCancelledDefaultExpansionStopsBeforeConstruction(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	leaf := wfv1.Template{Suspend: &wfv1.SuspendTemplate{}}
	w.Spec.TemplateDefaults = &wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &leaf}, {Name: "b", Inline: &leaf}}}}
	w.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Inline: &leaf}}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Prepare(ctx, requestFor(t, w), Policy{}, identityHooks())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func preparationReadCases(t *testing.T) {
	t.Helper()
	for _, state := range []string{"success", "missing", "failure"} {
		t.Run(state, func(t *testing.T) { preparationReadCase(t, state) })
	}
	t.Run("depth stop", func(t *testing.T) {
		reads := 0
		p := Policy{TemplateGetter: func(_ context.Context, name string) (*wfv1.WorkflowTemplate, error) {
			reads++
			obj := sharedObject(name)
			obj.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "next", TemplateRef: &wfv1.TemplateRef{Name: fmt.Sprintf("r%d", reads), Template: "work"}}}}}
			return obj, nil
		}}
		_, err := Prepare(t.Context(), requestFor(t, sharedConsumer("r0", "work")), p, identityHooks())
		requireCode(t, err, "depth")
		if reads != admissioncontract.MaxNestingDepth {
			t.Fatal(reads)
		}
	})
}

func preparationReadCase(t *testing.T, state string) {
	t.Helper()
	w := sharedConsumer("shared", "work")
	w.Spec.Templates[0].DAG.Tasks = append(w.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "again", TemplateRef: &wfv1.TemplateRef{Name: "shared", Template: "work"}})
	reads := 0
	p := Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) {
		reads++
		if state == "missing" {
			return nil, nil
		}
		if state == "failure" && reads == 1 {
			return nil, errors.New("reader unavailable")
		}
		return sharedObject("shared"), nil
	}}
	r := requestFor(t, w)
	for preparation := range 2 {
		result, err := Prepare(t.Context(), r, p, identityHooks())
		fails := state == "missing" || state == "failure" && preparation == 0
		assertReadPreparation(t, result, err, fails)
		if reads != preparation+1 {
			t.Fatalf("%d reads for %d preparations", reads, preparation+1)
		}

	}
}

func assertReadPreparation(t *testing.T, result *admissioncontract.PreparedAdmission, err error, fails bool) {
	t.Helper()
	if (err != nil) != fails {
		t.Fatal(err)
	}
	if !fails && len(result.View().Templates) != 1 {
		t.Fatal("snapshot count")
	}
	if fails {
		for _, path := range []string{`spec.templates["main"].dag.tasks[work]`, `spec.templates["main"].dag.tasks[again]`} {
			if !strings.Contains(err.Error(), path) {
				t.Fatal("call path missing", err)
			}
		}
	}
}

func TestPreparationCancellationPrecedesDocumentParsing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := requestFor(t, simpleWorkflow())
	r.Document = []byte("invalid")
	_, err := Prepare(ctx, r, Policy{}, identityHooks())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestGraphCancellationAfterReadStopsInspection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) {
		cancel()
		obj := sharedObject("shared")
		obj.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		return obj, nil
	}}
	w := sharedConsumer("shared", "work")
	if err := p.admit(ctx, w, requestFor(t, w), identityHooks()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestContainerSetPlanIgnoresContainerDefaults(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0] = wfv1.Template{Name: "main", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "work", Image: "image"}}}}}
	w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Command: []string{"oberth", "secretstore", "exec"}}}
	p, err := plannedSteps(t.Context(), w, admissioncontract.PrepareRequest{}, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Exact || len(p.Entries) != 1 || p.Entries[0].CredentialIntent {
		t.Fatal("inapplicable defaults changed containerSet intent", p)
	}
}

func TestPreparedExitSelectionDoesNotDeliverSuppressedSource(t *testing.T) {
	t.Parallel()
	w := sourceWorkflow()
	work := w.Spec.Templates[0]
	work.Name = "work"
	work.Inputs.Artifacts = []wfv1.Artifact{{Name: "source", Path: "/work/src", Mode: ptr.To(int32(0755))}}
	sourceArg := wfv1.Arguments{Artifacts: []wfv1.Artifact{{Name: "source", From: "{{inputs.artifacts.source}}"}}}
	unusedArg := wfv1.Arguments{Artifacts: []wfv1.Artifact{{Name: "source", From: "{{inputs.artifacts.other}}"}}}
	group := wfv1.Template{Name: "main", Inputs: wfv1.Inputs{Artifacts: []wfv1.Artifact{mainSource(), {Name: "other", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "authored"}}}}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "work", Arguments: sourceArg, OnExit: "cleanup", Hooks: wfv1.LifecycleHooks{"exit": {Template: "work", Arguments: unusedArg}}}}}}
	group.Inputs.Artifacts[0].Path = ""
	w.Spec.Templates = []wfv1.Template{group, work, {Name: "cleanup", Suspend: &wfv1.SuspendTemplate{}}}
	r := requestFor(t, w)
	bindings := []sourcecontract.Binding{{Template: "work", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: r.SourceReceipt.Revision}}
	p, err := Prepare(t.Context(), r, Policy{}, sourceHooks(bindings...))
	if err != nil {
		t.Fatal(err)
	}
	if got := planPaths(p.View().Plan); !reflect.DeepEqual(got, []string{"run", "run.onExit"}) {
		t.Fatal(got)
	}
}
