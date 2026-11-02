package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestParallelismAdmissionMatrixSharesAndReportsConcurrency(t *testing.T) {
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations: {oberth.ci/size: XL}
spec:
  activeDeadlineSeconds: 3600
  entrypoint: step-abc12345
  templates:
  - name: step-abc12345
    parallelism: 4
    steps:
    - - name: step-11111111
        template: step-11111111
        withSequence: {count: "18"}
  - name: step-11111111
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      resources:
        requests: {cpu: 2, memory: 6Gi}
        limits: {memory: 6Gi}
`)
	resolved, err := argoworkflow.ResolveRunSource(source, nil, argoworkflow.RunSourceBinding{RunID: strings.Repeat("a", 32), RepositoryID: 1, SHA: strings.Repeat("b", 40), File: ".oberth/build.yaml", Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := resolved.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
	if err != nil {
		t.Fatal(err)
	}
	g := newWeightedAdmission(6)
	g.resources.CPUMilli, g.resources.MemoryBytes = 12000, 32<<30
	d, fallback := g.selectDeclaredDemand(receipt)
	if d == nil || fallback != "" {
		t.Fatalf("bounded matrix fell back: %+v %s", d, fallback)
	}
	r := g.reserve(1)
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: strings.Repeat("a", 32), Size: periapsis.XL, Pods: d})
	defer release()
	charged := r.declaredResources()
	if charged.CPUMilli != 8400 || charged.MemoryBytes != 25<<30 || charged.Pods != 4 {
		t.Fatalf("lost executor allowance: %+v", charged)
	}
	small := acquireRequestWithinBudget(t, g.reserve(2), admissionRequest{Size: periapsis.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1}})
	defer small()
	api := &API{admission: &Scheduler{admission: g}}
	runs := []model.Run{{ID: r.runID, DisplayReason: "Priority normal: ordinary run."}}
	api.applyResourceMessages(t.Context(), runs)
	if !strings.Contains(runs[0].ResourceMessage, "effective concurrency: 4 pods") || runs[0].DisplayReason != "Priority normal: ordinary run." {
		t.Fatalf("missing separate concurrency notice: %+v", runs[0])
	}
}

func TestParallelismNoticeSurvivesCreateRecovery(t *testing.T) {
	f := newConcurrencyFixture(t, periapsis.M, 3)
	run := model.Run{ID: strings.Repeat("a", 32), JobName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
	d := &argoworkflow.PodDemand{CPUMilli: 4000, MemoryBytes: 4 << 30, Pods: 4, Assumptions: argoworkflow.PodPeakDynamic | argoworkflow.PodPeakNested}
	if err := f.scheduler.recordCreateAdmission(run, run.JobName, periapsis.M, argoworkflow.VMIDemand{}, d); err != nil {
		t.Fatal(err)
	}
	if _, err := f.scheduler.restoreCreateGrant(run, nil); err != nil {
		t.Fatal(err)
	}
	if message := f.scheduler.ResourceAdmissionMessage(run.ID); !strings.Contains(message, "effective concurrency: 4 pods") || !strings.Contains(message, "item count is unknown") || !strings.Contains(message, "full inner fan-out demand") {
		t.Fatalf("restored concurrency missing: %q", message)
	}
}
