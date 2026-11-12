package app

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestQueuedPodDemandUsesConcurrentRequestsAtExactCommit(t *testing.T) {
	source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: root
  templates:
  - name: root
    dag:
      tasks:
      - {name: first, template: first}
      - {name: second, template: second}
      - {name: last, template: last, dependencies: [first, second]}
  - name: first
    container:
      image: example.invalid/runner
      resources:
        requests: {cpu: 4900m, memory: 8Gi}
        limits: {cpu: 20, memory: 64Gi}
  - name: second
    container:
      image: example.invalid/runner
      resources:
        requests: {cpu: 5, memory: 8Gi}
        limits: {cpu: 20, memory: 64Gi}
  - name: last
    container:
      image: example.invalid/runner
      resources:
        requests: {cpu: 2, memory: 4Gi}
        limits: {cpu: 20, memory: 64Gi}
`
	for _, parallel := range []bool{true, false} {
		t.Run(map[bool]string{true: "parallel", false: "serial"}[parallel], func(t *testing.T) {
			document := source
			cpu, memory := int64(10000), int64(16<<30)
			if !parallel {
				document = strings.Replace(document, "  entrypoint: root", "  entrypoint: root\n  parallelism: 1", 1)
				cpu, memory = 5050, 8<<30
			}
			blobs := &resourceBlobStub{source: []byte(document)}
			jobs := &ArgoJobs{resourceBlobs: blobs}
			request := service.WorkflowRequest{Run: model.Run{ID: strings.Repeat("a", 32), SHA: strings.Repeat("b", 40), TestedSHA: strings.Repeat("c", 40), RefKind: model.RefBranch, Trigger: "branch"}, Repository: model.Repository{ID: 1, Name: "repo"}, UpstreamName: "upstream", UpstreamOrg: "org"}
			demand, err := jobs.RunPodDemand(t.Context(), request)
			if err != nil || demand == nil || demand.CPUMilli != cpu || demand.MemoryBytes != memory || blobs.sha != request.Run.TestedSHA {
				t.Fatalf("demand=%+v err=%v SHA=%s", demand, err, blobs.sha)
			}
			jobs.config.StepCPURequestMaxMilli = 3000
			jobs.config.CPUAdmission.Milli = 4000
			capped, err := jobs.RunPodDemand(t.Context(), request)
			want := int64(4000)
			if !parallel {
				want = 3050
			}
			if err != nil || capped == nil || capped.CPUMilli != want {
				t.Fatalf("uncapped queue demand: %+v %v", capped, err)
			}
			jobs.config.StepCPURequestMaxMilli = 0
			jobs.config.CPUAdmission.Milli = 0
			// The preview uses the same request defaults as submission.
			blobs.source = []byte(strings.ReplaceAll(document, "requests: {cpu: 5, memory: 8Gi}", "requests: {cpu: 5}"))
			demand, err = jobs.RunPodDemand(t.Context(), request)
			if err != nil || demand == nil || demand.CPUMilli != cpu {
				t.Fatalf("missing requests: %+v %v", demand, err)
			}
		})
	}
}
