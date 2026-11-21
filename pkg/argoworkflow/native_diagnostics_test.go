package argoworkflow

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

func TestIndependentLocalAndSharedNativeErrorsKeepTheirLocations(t *testing.T) {
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks[0].Arguments.Parameters = []wfv1.Parameter{{Name: "missing-value"}}
	w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "bad", Container: &corev1.Container{Image: "image"}, Suspend: &wfv1.SuspendTemplate{}})
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	assertNativeLocations(t, err, []string{`spec.templates["main"].dag.tasks[run].arguments.parameters[missing-value].value`, `spec.templates["bad"]`})
	w = sharedConsumer("a", "work")
	w.Spec.Templates[0].DAG.Tasks = append(w.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "second", TemplateRef: &wfv1.TemplateRef{Name: "b", Template: "work"}})
	a, b := sharedObject("a"), sharedObject("b")
	a.Spec.Templates[0].Suspend = &wfv1.SuspendTemplate{}
	b.Spec.Templates[0].Suspend = &wfv1.SuspendTemplate{}
	_, err = Prepare(t.Context(), requestFor(t, w), Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"a": a, "b": b})}, identityHooks())
	assertNativeLocations(t, err, []string{`templates[a].templates["work"]`, `templates[b].templates["work"]`})
	for _, caller := range []string{`spec.templates["main"].dag.tasks[work]`, `spec.templates["main"].dag.tasks[second]`} {
		if !strings.Contains(err.Error(), caller) {
			t.Fatal("caller missing", err)
		}
	}
}
func assertNativeLocations(t *testing.T, err error, want []string) {
	t.Helper()
	findings := admissionFindings(t, err)
	if len(findings) != len(want) {
		t.Fatal("missing or duplicate native findings", err)
	}
	for _, path := range want {
		found := false
		for _, f := range findings {
			found = found || f.Path == path && f.Code == "native"
		}
		if !found {
			t.Fatal("missing", path, err)
		}
	}
}

func TestNativeStepAndWorkflowArgumentPaths(t *testing.T) {
	for _, root := range []bool{false, true} {
		w := planDAG()
		want := `spec.arguments.parameters[missing].value`
		if root {
			w.Spec.Arguments.Parameters = []wfv1.Parameter{{Name: "missing"}}
		} else {
			w.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "work", Template: "leaf", Arguments: wfv1.Arguments{Parameters: []wfv1.Parameter{{Name: "missing"}}}}}}}}
			want = `spec.templates["main"].steps[0]["work"].arguments.parameters[missing].value`
		}
		_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
		assertNativeLocations(t, err, []string{want})
	}
}

func TestCanceledNativeWaiterReturnsBeforeGateRelease(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(fmt.Sprint(prepare), func(t *testing.T) {
			nativeSchemaGate <- struct{}{}
			defer func() { <-nativeSchemaGate }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := requestFor(t, planDAG())
			done := make(chan error, 1)
			go func() {
				if prepare {
					_, err := Prepare(ctx, request, Policy{}, identityHooks())
					done <- err
					return
				}
				done <- ValidateNativeSchema(ctx, planDAG(), admissioncontract.PrepareRequest{})
			}()
			awaitNativeWaiter(t)
			cancel()
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-timer.C:
				t.Fatal("canceled validation waited for gate release")
			}
		})
	}
}
func awaitNativeWaiter(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal("preparation did not reach native gate")
		default:
		}
		stack := make([]byte, 256<<10)
		n := runtime.Stack(stack, true)
		if strings.Contains(string(stack[:n]), "argoworkflow.validateNativeSchema(") {
			return
		}
		runtime.Gosched()
	}
}

func TestAlreadyCanceledNativeCallNeverTakesTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ValidateNativeSchema(ctx, simpleWorkflow(), admissioncontract.PrepareRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(nativeSchemaGate) != 0 {
		t.Fatal("gate retained")
	}
}

func TestNativeInvalidTailHasBoundedResponseTime(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Templates = nil
	for i := range 1500 {
		name := fmt.Sprintf("t%d", i)
		if i == 0 {
			name = "main"
		}
		w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: name, Container: &corev1.Container{Image: "image", Command: []string{"true"}}})
	}
	w.Spec.Templates[1499].Suspend = &wfv1.SuspendTemplate{}
	r := requestFor(t, w)
	t.Logf("invalid-tail bytes=%d templates=%d", len(r.Document), len(w.Spec.Templates))
	done := make(chan error, 1)
	go func() { _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); done <- err }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		assertNativeLocations(t, err, []string{`spec.templates["t1499"]`})
	case <-timer.C:
		t.Fatal("1500-template native diagnostic exceeded 5s")
	}
}

func TestNativeDiagnosticWorkBoundary(t *testing.T) {
	for _, extra := range []bool{false, true} {
		w := simpleWorkflow()
		for i := range 255 {
			w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, wfv1.Parameter{Name: fmt.Sprintf("p%d", i), Value: wfv1.AnyStringPtr("value")})
		}
		for i := 1; i < 256; i++ {
			w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: fmt.Sprintf("t%d", i), Container: &corev1.Container{Image: "image"}})
		}
		if extra {
			w.Spec.Templates[255].Container = nil
			w.Spec.Templates[255].ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "a", Image: "image"}}, {Container: corev1.Container{Name: "b", Image: "image"}}}}
		}
		err := ValidateNativeSchema(t.Context(), w, admissioncontract.PrepareRequest{})
		assertBoundary(t, err, !extra)
		if extra {
			requireLimit(t, err, "native_work", "65,536")
		}
	}
}

func TestInspectionExhaustionAddsNoMissingTargetFinding(t *testing.T) {
	w := simpleWorkflow()
	inline := &wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "leaf", Template: "leaf"}}}}
	w.Spec.TemplateDefaults = &wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: inline}, {Name: "b", Inline: inline}}}}
	w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "leaf", Template: "leaf"}}}}, {Name: "leaf", Suspend: &wfv1.SuspendTemplate{}}}
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	requireLimit(t, err, "inspection", "65,536")
	for _, f := range admissionFindings(t, err) {
		if f.Code == "reference" || f.Code == "local_template" {
			t.Fatal("secondary missing-target error after budget exhaustion", err)
		}
	}
}
