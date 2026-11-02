package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestDeclaredPodSubmissionBindsExactDemandAndRejectsChanges(t *testing.T) {
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"example.invalid/"}
	r := resourceBoundsRequest(resourceBoundsSource("      resources:\n        requests: {cpu: 1000m, memory: 1Gi}\n"))
	r.PodDemand = &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1}
	wf, err := Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := readPodDemand(wf)
	if err != nil || d == nil || *d != *r.PodDemand {
		t.Fatalf("submission binding: %+v %v", d, err)
	}
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
