package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

type resourceBlobStub struct {
	source []byte
	sha    string
}

func TestCurrentDeclarationOmittedLimitsCannotAcquireFiniteAdvice(t *testing.T) {
	for _, missing := range []string{"", "memory", "ephemeral-storage"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			limits := "{memory: 2Gi, ephemeral-storage: 512Mi}"
			if missing == "memory" {
				limits = "{ephemeral-storage: 512Mi}"
			} else if missing == "ephemeral-storage" {
				limits = "{memory: 2Gi}"
			}
			b := &resourceBlobStub{source: []byte(fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata: {generateName: advice-}
spec:
  entrypoint: root
  templates:
  - name: root
    dag:
      tasks:
      - {name: build, template: compile}
  - name: compile
    container:
      image: example.invalid/repo-01
      resources:
        limits: %s
`, limits))}
			jobs := &ArgoJobs{resourceBlobs: b}
			d, err := jobs.ResourceDeclarations(t.Context(), service.WorkflowRequest{Run: model.Run{ID: strings.Repeat("a", 32), SHA: strings.Repeat("b", 40), TestedSHA: strings.Repeat("c", 40), RefKind: model.RefBranch, Trigger: "branch"}, Repository: model.Repository{ID: 1, Name: "repo-01"}, UpstreamName: "repo-01", UpstreamOrg: "repo-01"})
			if err != nil || len(d) != 1 || b.sha != strings.Repeat("c", 40) {
				t.Fatal("declaration did not come from tested source", d, err)
			}
			if missing != "" {
				if _, exists := d[0].Resources["limits"][missing]; exists {
					t.Fatal("declaration reader manufactured a current limit")
				}
			}
			var rows []measured.Observation
			at := time.Now().UTC()
			p := func(n int64) *int64 { return &n }
			for i := range 5 {
				coverage := model.MetricCoverage{Count: 4, FirstAt: at.Add(-10 * time.Second), LastAt: at}
				code := int32(0)
				v := model.StepResourceUsage{Version: 2, Source: "kubelet-cadvisor", SampledLowerBound: true, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", NodeName: "node-a", FirstObservedAt: coverage.FirstAt, LastObservedAt: at, SourceAt: at, ContainerKind: "app", PodKind: "workflow", RunID: fmt.Sprintf("%032x", i+1), PodUID: "22222222-2222-4222-8222-222222222222", ContainerID: "containerd://" + strings.Repeat("b", 64), ContainerName: "main", Stage: d[0].Stage, Step: "compile", TemplateName: d[0].Template, Samples: 4, Terminal: true, Reason: "Completed", ExitCode: &code, ContainerStartedAt: coverage.FirstAt, ContainerFinishedAt: at, PodStartedAt: coverage.FirstAt, PodFinishedAt: at, MemoryCoverage: coverage, CPUCoverage: coverage, StorageCoverage: coverage, MemoryPeakBytes: p(256 << 20), CPUTimeNS: p(5e9), CPUTimeAt: at, CPUPeakNanoCores: p(1e9), EphemeralStoragePeakBytes: p(100 << 20), CPURequestNanoCores: p(0), MemoryRequestBytes: p(0), EphemeralRequestBytes: p(0), CPULimitNanoCores: p(0), MemoryLimitBytes: p(1 << 30), EphemeralLimitBytes: p(512 << 20), PodEphemeralLimitBytes: p(512 << 20)}
				if err := v.Validate(); err != nil {
					t.Fatal("invalid observation fixture", err)
				}
				rows = append(rows, measured.Observation{Usage: v, Template: d[0].Template, Owner: "repo-01"})
			}
			step := measured.Build(rows, d)[0]
			if (step.Suggested == nil) != (missing != "") || step.MemoryPeakBytes.Count != 5 {
				t.Fatalf("missing=%s produced wrong advice or lost metrics: %+v", missing, step)
			}
		})
	}
}

func (b *resourceBlobStub) ReadOptionalBlob(_ context.Context, _ string, sha, _ string, _ int) ([]byte, bool, error) {
	b.sha = sha
	return b.source, true, nil
}
func (b *resourceBlobStub) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "", nil, false, nil
}
func TestRunResourcesDeclarationsUseExactTestedCommitAndOnlyNumbers(t *testing.T) {
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: numeric-
spec:
  entrypoint: root
  activeDeadlineSeconds: 600
  templates:
  - name: root
    dag:
      tasks:
      - name: step-12345678
        template: step-abc12345
  - name: step-abc12345
    activeDeadlineSeconds: 120
    container:
      image: example.invalid/repo-01
      command: [echo, SECRET-COMMAND]
      env:
      - name: SECRET-ENV
        value: hidden
      resources:
        requests: {cpu: 200m, memory: 128Mi}
        limits: {memory: 512Mi}
`)
	b := &resourceBlobStub{source: source}
	jobs := &ArgoJobs{resourceBlobs: b}
	got, e := jobs.ResourceDeclarations(t.Context(), service.WorkflowRequest{Run: model.Run{ID: strings.Repeat("a", 32), SHA: strings.Repeat("b", 40), TestedSHA: strings.Repeat("c", 40), RefKind: model.RefBranch, Trigger: "branch"}, Repository: model.Repository{ID: 1, Name: "repo-01"}, UpstreamName: "repo-01", UpstreamOrg: "repo-01"})
	if e != nil {
		t.Fatal(e)
	}
	if b.sha != strings.Repeat("c", 40) || len(got) != 1 || got[0].Template != "step-abc12345" || got[0].Stage != "step-12345678" || got[0].Resources["requests"]["cpu"] != "200m" || got[0].ActiveDeadlineSeconds == nil || *got[0].ActiveDeadlineSeconds != 120 {
		t.Fatalf("declarations: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "hidden") {
		t.Fatal("non-numeric spec fields leaked")
	}
}
