package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestDeclaredPodSubmissionBindsExactDemandAndRejectsChanges(t *testing.T) {
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"example.invalid/"}
	r := resourceBoundsRequest(resourceBoundsSource("      resources:\n        requests: {cpu: 1000m, memory: 1Gi}\n        limits: {memory: 1Gi}\n"))
	r.PodDemand = &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1}
	wf, err := Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := readPodDemand(wf)
	if err != nil || d == nil || *d != *r.PodDemand {
		t.Fatalf("submission binding: %+v %v", d, err)
	}
	source := string(r.Source)
	r.Source = []byte(strings.Replace(source, "        limits: {memory: 1Gi}\n", "", 1))
	if _, err := Build(cfg, r); err == nil || !strings.Contains(err.Error(), "changed after reservation") {
		t.Fatalf("removed memory limit retained numeric reservation: %v", err)
	}
	r.Source = []byte(source)
	r.Source = []byte(strings.Replace(string(r.Source), "cpu: 1000m", "cpu: 2000m", 1))
	if _, err := Build(cfg, r); err == nil || !strings.Contains(err.Error(), "changed after reservation") {
		t.Fatalf("source change: %v", err)
	}
	r.PodDemand = nil
	r.Source = []byte(strings.Replace(string(r.Source), "oberth.ci/size: XL", `oberth.ci/size: XL
    oberth.ci/declared-pod-demand: '{"cpu_milli":1,"memory_bytes":1,"pods":1}'`, 1))
	wf, err = Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Annotations[podDemandAnnotation] != "" {
		t.Fatal("pipeline forged server reservation")
	}
}

func TestResourceFallbackIsServerOwnedAndRestored(t *testing.T) {
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"example.invalid/"}
	r := resourceBoundsRequest(resourceBoundsSource("      resources:\n        requests: {cpu: 1, memory: 1Gi}\n"))
	r.ResourceFallback = model.ResourceFallbackBudget
	wf, err := Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := readResourceFallback(wf)
	if err != nil || fallback != model.ResourceFallbackBudget {
		t.Fatal("fallback submission notice lost")
	}
	r.ResourceFallback = model.ResourceFallbackGraph
	r.Source = []byte(strings.Replace(string(r.Source), "  entrypoint: step-abc12345", "  entrypoint: step-abc12345\n  onExit: step-11111111", 1) + "  - name: step-11111111\n    container:\n      image: example.invalid/repo-01@sha256:" + strings.Repeat("a", 64) + "\n      resources:\n        requests: {cpu: 1, memory: 1Gi}\n")
	wf, err = Build(cfg, r)
	if err != nil {
		t.Fatalf("graph fallback refused at trusted submission: %v", err)
	}
	if fallback, err = readResourceFallback(wf); err != nil || fallback != model.ResourceFallbackGraph {
		t.Fatal("graph fallback notice lost at trusted submission")
	}
	wf, req := adoptionWorkflow()
	if err = stampResourceFallback(wf, model.ResourceFallbackGraph); err != nil {
		t.Fatal(err)
	}
	c := &Controller{config: Config{Namespace: wf.Namespace}}
	binding, err := c.bindAdoption(wf, req)
	if err != nil || binding.Fallback != model.ResourceFallbackGraph {
		t.Fatal("fallback adoption lost")
	}
	changed := wf.DeepCopy()
	_ = stampResourceFallback(changed, model.ResourceFallbackBudget)
	if binding.matches(changed) {
		t.Fatal("fallback binding changed undetected")
	}
}

func TestDeclaredPodAdoptionRestoresSealedDemand(t *testing.T) {
	wf, req := adoptionWorkflow()
	d := argoworkflow.PodDemand{CPUMilli: 6000, MemoryBytes: 8 << 30, Pods: 2}
	stampPodDemand(wf, &d)
	c := &Controller{config: Config{Namespace: wf.Namespace}}
	binding, err := c.bindAdoption(wf, req)
	if err != nil || binding.Pods != d {
		t.Fatalf("adoption: %+v %v", binding, err)
	}
	changed := wf.DeepCopy()
	d.CPUMilli++
	stampPodDemand(changed, &d)
	if binding.matches(changed) {
		t.Fatal("adopted reservation annotation changed undetected")
	}
	changed.Annotations[podDemandAnnotation] = "{}"
	if _, err := c.bindAdoption(changed, req); err == nil {
		t.Fatal("malformed reservation restored")
	}
}

func TestParallelismNoticeRetainedWithWorkflowTiming(t *testing.T) {
	wf, _, _, _ := diagnosticFixture()
	wf.Annotations[identityAnnotation] = strings.Repeat("c", 64)
	d := argoworkflow.PodDemand{CPUMilli: 4000, MemoryBytes: 4 << 30, Pods: 4}
	stampPodDemand(wf, &d)
	recorder := &timingRecorderStub{}
	controller := &Controller{timings: recorder}
	controller.retainWorkflowTiming(t.Context(), wf)
	if len(recorder.workflows) != 1 || recorder.workflows[0].Resources == nil {
		t.Fatal("missing durable resource metadata")
	}
	retained := recorder.workflows[0]
	if message := retained.Resources.Message(retained.RunID); !strings.Contains(message, "effective concurrency: 4 pods") {
		t.Fatalf("concurrency notice lost: %q", message)
	}
}

func TestParallelismChangeCannotReuseReservation(t *testing.T) {
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"example.invalid/"}
	source := resourceBoundsSource("      resources:\n        requests: {cpu: 1000m, memory: 1Gi}\n        limits: {memory: 1Gi}\n")
	source = strings.Replace(source, "  entrypoint: step-abc12345", "  entrypoint: step-11111111", 1)
	source += `  - name: step-11111111
    parallelism: 4
    steps:
    - - name: step-abc12345
        template: step-abc12345
        withSequence: {count: "18"}
`
	r := resourceBoundsRequest(source)
	r.PodDemand = &argoworkflow.PodDemand{CPUMilli: 4000, MemoryBytes: 4 << 30, Pods: 4}
	wf, err := Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := readPodDemand(wf); err != nil || d == nil || *d != *r.PodDemand {
		t.Fatalf("wrong submission demand: %+v %v", d, err)
	}
	r.Source = []byte(strings.Replace(source, "parallelism: 4", "parallelism: 5", 1))
	if _, err := Build(cfg, r); err == nil || !strings.Contains(err.Error(), "changed after reservation") {
		t.Fatalf("changed parallelism reused reservation: %v", err)
	}
}
