package argojob

import (
	"strings"
	"testing"

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
