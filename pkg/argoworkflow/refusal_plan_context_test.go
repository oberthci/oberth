package argoworkflow

import (
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestRefusalPlannerRecursiveBoundaryContext(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "A-first"
		if reverse {
			name = "B-first"
		}
		t.Run(name, func(t *testing.T) {
			calls := []wfv1.DAGTask{{Name: "entryA", Template: "A"}, {Name: "entryB", Template: "B"}}
			if reverse {
				calls[0], calls[1] = calls[1], calls[0]
			}
			workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
				{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: calls}},
				{Name: "A", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "entryB", Template: "B"}, {Name: "runA", Template: "leaf"}}}},
				{Name: "B", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "viaA", Template: "A"}}}},
				{Name: "leaf", Container: &corev1.Container{Image: "registry.example.test/runner:1", Command: []string{"true"}}},
			}}}
			if err := Admit(workflow, Policy{}); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanSteps(workflow)
			if err != nil {
				t.Fatalf("distinct recursive boundaries refused: %v", err)
			}
			got := map[string]bool{}
			for _, row := range plan.Steps {
				got[row.Stage+"/"+row.Step] = true
			}
			if len(plan.Steps) != 2 || !got["entryA/runA"] || !got["viaA/runA"] {
				t.Fatalf("context-dependent rows omitted: %+v", plan.Steps)
			}
			if !plan.BackEdgeCut {
				t.Fatal("recursive plan did not expose its back-edge cut")
			}
			rows, err := PlannedSteps(workflow)
			if err != nil || len(rows) != 2 {
				t.Fatalf("row API disagrees with plan: %+v %v", rows, err)
			}
		})
	}
}

func TestRefusalPlannerCycleMetadataAndBounds(t *testing.T) {
	emptyCycle := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
		{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "empty", Template: "A"}}}},
		{Name: "A", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "b", Template: "B"}}}},
		{Name: "B", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "A"}}}},
	}}}
	blocked := refusalCompositeGraph("complete-13")
	for i := range blocked.Spec.Templates {
		blocked.Spec.Templates[i].Steps[0].Steps = append(blocked.Spec.Templates[i].Steps[0].Steps, wfv1.WorkflowStep{Name: "back", Template: "root"})
	}
	blocked.Spec.Entrypoint = "root"
	blocked.Spec.Templates = append(blocked.Spec.Templates,
		wfv1.Template{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "through", Template: "t0"}, {Name: "run", Template: "leaf"}}}},
		wfv1.Template{Name: "leaf", Container: &corev1.Container{Image: "registry.example.test/runner:1"}},
	)
	blankComplete := refusalCompositeGraph("complete-13")
	for i := range blankComplete.Spec.Templates {
		blankComplete.Spec.Templates[i].Steps[0].Steps = append(blankComplete.Spec.Templates[i].Steps[0].Steps, wfv1.WorkflowStep{Name: " ", Template: "leaf"}, wfv1.WorkflowStep{Name: "", Template: "leaf"})
	}
	leaf := wfv1.Template{Name: "leaf", Container: &corev1.Container{Image: "registry.example.test/runner:1"}}
	blankComplete.Spec.Templates = append(blankComplete.Spec.Templates, leaf)
	blankDiamond := refusalCompositeGraph("diamond-40")
	blankDiamond.Spec.Templates[40].DAG.Tasks = []wfv1.DAGTask{{Name: "again", Template: "t0"}, {Name: " ", Template: "leaf"}}
	blankDiamond.Spec.Templates = append(blankDiamond.Spec.Templates, leaf)
	for _, test := range []struct {
		name     string
		workflow *wfv1.Workflow
		cut      bool
		rows     int
	}{
		{"complete-13", refusalCompositeGraph("complete-13"), true, 0},
		{"diamond-40", refusalCompositeGraph("diamond-40"), false, 0},
		{"empty-inner-cycle", emptyCycle, true, 0},
		{"ancestor-only-leaf", blocked, true, 1},
		{"factorial-13-blank-leaf", blankComplete, true, 0},
		{"diamond-40-back-edge-blank-leaf", blankDiamond, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			type result struct {
				plan StepPlan
				err  error
			}
			done := make(chan result, 1)
			go func() { plan, err := PlanSteps(test.workflow); done <- result{plan, err} }()
			select {
			case got := <-done:
				if got.err != nil || got.plan.BackEdgeCut != test.cut || len(got.plan.Steps) != test.rows {
					t.Fatalf("plan=%+v error=%v", got.plan, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("context-aware composite planning exceeded two seconds")
			}
		})
	}
}

func TestRefusalPlannerCycleFreeDuplicatesRemainRefused(t *testing.T) {
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
		{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "left", Template: "left"}, {Name: "right", Template: "right"}}}},
		{Name: "left", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "shared", Template: "shared"}}}},
		{Name: "right", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "shared", Template: "shared"}}}},
		{Name: "shared", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "leaf"}}}},
		{Name: "leaf", Container: &corev1.Container{Image: "registry.example.test/runner:1"}},
	}}}
	if _, err := PlanSteps(workflow); err == nil || !strings.Contains(err.Error(), "duplicate step key") {
		t.Fatalf("cycle-free repeated rows were not refused: %v", err)
	}
}

func TestRefusalPlannerResolvesInlineBeforeCycleCut(t *testing.T) {
	leaf := &wfv1.Template{Container: &corev1.Container{Image: "registry.example.test/runner:1"}}
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "root", Inline: leaf}}}}}}}
	if err := Admit(workflow, Policy{}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanSteps(workflow)
	if err != nil || len(plan.Steps) != 1 || plan.Steps[0].Step != "run" || plan.BackEdgeCut {
		t.Fatalf("inline Pod was mistaken for recursive composite: %+v %v", plan, err)
	}
}

func TestRefusalPlannerHardVisitBudget(t *testing.T) {
	// A large, empty recursive graph exercises search work independently of the
	// number of emitted rows and of whether any subtree can be memoized.
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "t0"}}
	for i := 0; i < 300; i++ {
		template := wfv1.Template{Name: fmt.Sprintf("t%d", i), DAG: &wfv1.DAGTemplate{}}
		for j := 0; j < 300; j++ {
			template.DAG.Tasks = append(template.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("s%d", j), Template: fmt.Sprintf("t%d", j)})
		}
		workflow.Spec.Templates = append(workflow.Spec.Templates, template)
	}
	done := make(chan error, 1)
	go func() { _, err := PlanSteps(workflow); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "step planning exceeded the") || !strings.Contains(err.Error(), "visit limit") {
			t.Fatalf("missing plain planner safety refusal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hard visit budget did not bound planning")
	}
}
