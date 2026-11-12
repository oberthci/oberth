package argojob

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestSchedulingCPUCapPreservesBurstAndCountsHelpers(t *testing.T) {
	source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      resources:
        requests: {cpu: 5400m, memory: 256Mi}
        limits: {cpu: 8, memory: 1Gi}
`
	wf, err := argoworkflow.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	demand, err := SchedulingDemand(wf, argoworkflow.CPURequestPolicy{MaxMilli: 3000})
	if err != nil || demand.CPUMilli != 3050 {
		t.Fatalf("demand=%+v err=%v", demand, err)
	}
	c := testConfig()
	c.StepCPURequestMaxMilli = 3000
	request := testRequest(pipeline.TriggerCI, source)
	request.RunTrigger = argoworkflow.RunTriggerBranch
	request.PipelineFile = ".oberth/build.yaml"
	resolved, err := argoworkflow.ResolveRunSource(request.Source, nil, argoworkflow.RunSourceBinding{RunID: request.RunID, RepositoryID: request.RepositoryID, SHA: request.SHA, File: request.PipelineFile, Trigger: request.RunTrigger})
	if err != nil {
		t.Fatal(err)
	}
	request.DemandReceipt, err = resolved.Receipt(argoworkflow.Policy{ServerDeadline: true})
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build(c, request)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := readPodDemand(built)
	if err != nil || bound == nil || bound.CPUMilli != demand.CPUMilli {
		t.Fatalf("restart demand=%+v want=%+v err=%v", bound, demand, err)
	}
	found := false
	for _, tmpl := range built.Spec.Templates {
		if tmpl.Container != nil && tmpl.Name == "main" {
			found = true
			r := tmpl.Container.Resources
			if r.Requests.Cpu().MilliValue() != 3000 || r.Limits.Cpu().MilliValue() != 8000 {
				t.Fatal(r)
			}
		}
	}
	if !found {
		t.Fatal("missing main")
	}
	if wf.Spec.Templates[0].Container.Resources.Requests.Cpu().String() != "5400m" {
		t.Fatal("sizing mutated source")
	}
}
func TestServerClassesReachArgoPriority(t *testing.T) {
	for _, tc := range []struct {
		priority model.RunPriority
		want     int32
	}{{model.PriorityLow, -1}, {model.PriorityStandard, 0}, {model.PriorityPromotion, 1}, {model.PriorityHigh, 2}} {
		request := testRequest(pipeline.TriggerCI, greedyDocument)
		request.Priority = tc.priority
		wf, err := Build(testConfig(), request)
		if err != nil {
			t.Fatal(err)
		}
		got := int32(0)
		if wf.Spec.Priority != nil {
			got = *wf.Spec.Priority
		}
		if got != tc.want {
			t.Fatalf("%s: %d", tc.priority, got)
		}
	}
	request := testRequest(pipeline.TriggerRelease, strings.ReplaceAll(greedyDocument, "ci:", "release:"))
	wf, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Priority == nil || *wf.Spec.Priority < 1 {
		t.Fatal("release lost priority")
	}
}

func TestSchedulingHooksAndExitHandlersAreBounded(t *testing.T) {
	base := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: root
  templates:
  - name: root
    dag:
      tasks:
      - name: work
        template: work
  - name: work
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      resources:
        requests: {cpu: 1, memory: 256Mi}
        limits: {cpu: 4, memory: 1Gi}
`
	shapes := map[string]string{
		"exit":      strings.Replace(base, "  entrypoint: root", "  entrypoint: root\n  onExit: work", 1),
		"hook":      strings.Replace(base, "        template: work", "        template: work\n        hooks:\n          exit: {template: work}", 1),
		"withParam": strings.Replace(base, "        template: work", "        template: work\n        withParam: '{{workflow.parameters.items}}'", 1),
	}
	shapes["step-hook"] = strings.Replace(base, "    dag:\n      tasks:\n      - name: work\n        template: work", "    steps:\n    - - name: work\n        template: work\n        hooks:\n          exit: {template: work}", 1)
	for name, source := range shapes {
		t.Run(name, func(t *testing.T) {
			request := testRequest(pipeline.TriggerCI, source)
			request.RunTrigger = argoworkflow.RunTriggerBranch
			request.PipelineFile = ".oberth/build.yaml"
			resolved, err := argoworkflow.ResolveRunSource(request.Source, nil, argoworkflow.RunSourceBinding{RunID: request.RunID, RepositoryID: request.RepositoryID, SHA: request.SHA, File: request.PipelineFile, Trigger: request.RunTrigger})
			if err != nil {
				t.Fatal(err)
			}
			request.DemandReceipt, err = resolved.Receipt(argoworkflow.Policy{ServerDeadline: true})
			if err != nil {
				t.Fatal(err)
			}
			config := testConfig()
			config.StepCPURequestMaxMilli = 3000
			wf, err := Build(config, request)
			if err != nil {
				t.Fatalf("valid graph rejected: %v", err)
			}
			demand, err := readPodDemand(wf)
			if err != nil {
				t.Fatal(err)
			}
			if name == "withParam" {
				if demand != nil {
					t.Fatal("unbounded withParam should stay unknown")
				}
			} else {
				want := int64(2100)
				if name == "exit" {
					want = 1050
				}
				if demand == nil || demand.CPUMilli != want || !demand.Valid() {
					t.Fatalf("handlers not bounded: %+v", demand)
				}
			}
			bounded := strings.Replace(source, "  entrypoint: root", "  entrypoint: root\n  parallelism: 1", 1)
			raw, err := argoworkflow.Decode([]byte(bounded))
			if err != nil {
				t.Fatal(err)
			}
			single, err := SchedulingDemand(raw, argoworkflow.CPURequestPolicy{MaxMilli: 3000})
			if err != nil || single == nil || single.CPUMilli != 1050 {
				t.Fatalf("single Pod parallelism did not bound every template: %+v %v", single, err)
			}
		})
	}
}

func TestSchedulingBoundScansLargerExitAndHookTemplates(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", OnExit: "cleanup", Templates: []wfv1.Template{
		{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "main", Template: "work"}}}},
		{Name: "work", Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}},
		{Name: "cleanup", Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}},
	}}}
	policy := argoworkflow.CPURequestPolicy{MaxMilli: 3000}
	demand, err := SchedulingDemand(wf, policy)
	if err != nil || demand == nil || demand.CPUMilli != 3050 {
		t.Fatalf("exit not sequentially bounded: %+v %v", demand, err)
	}
	wf.Spec.OnExit = ""
	wf.Spec.Templates[0].DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"running": {Template: "cleanup", Expression: "true"}}
	demand, err = SchedulingDemand(wf, policy)
	if err != nil || demand == nil || demand.CPUMilli != 4100 {
		t.Fatalf("concurrent hook omitted: %+v %v", demand, err)
	}
	one := int64(1)
	wf.Spec.Parallelism = &one
	demand, err = SchedulingDemand(wf, policy)
	if err != nil || demand == nil || demand.CPUMilli != 3050 {
		t.Fatalf("parallelism omitted large hook: %+v %v", demand, err)
	}
	// Exceed the static graph work bound: the parallelism fallback must still
	// scan the larger exit template rather than only the main entrypoint.
	wf.Spec.OnExit = "cleanup"
	wf.Spec.Templates[0].DAG.Tasks = nil
	for i := range 300 {
		wf.Spec.Templates[0].DAG.Tasks = append(wf.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("work-%d", i), Template: "work"})
	}
	demand, err = SchedulingDemand(wf, policy)
	if err != nil || demand == nil || demand.CPUMilli != 3050 {
		t.Fatalf("bounded fallback ignored exit: %+v %v", demand, err)
	}
}
