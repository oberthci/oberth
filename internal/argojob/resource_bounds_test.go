package argojob

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func resourceBoundsSource(resources string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: XL
spec:
  entrypoint: step-abc12345
  activeDeadlineSeconds: 3600
  templates:
  - name: step-abc12345
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      command: [/bin/true]
` + resources
}
func resourceBoundsRequest(source string) Request {
	r := testRequest(periapsis.TriggerCI, source)
	r.Repo = "repo-01"
	r.UpstreamName = "repo-01"
	r.UpstreamOrg = "repo-01"
	r.ApprovedSecrets = map[string]bool{}
	r.ResourceMaximum = periapsis.L
	return r
}
func TestResourceBoundsBuildUsesTrustedMaximumAndHonorsExactValues(t *testing.T) {
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	source := resourceBoundsSource("      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi, ephemeral-storage: 128Mi}\n")
	r := resourceBoundsRequest(source)
	wf, e := Build(config, r)
	if e != nil {
		t.Fatal(e)
	}
	before, e := argoworkflow.Decode([]byte(source))
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(wf.Spec.Templates[0].Container.Resources, before.Spec.Templates[0].Container.Resources) {
		t.Fatal("explicit resources changed")
	}
	if _, ok := wf.Spec.Templates[0].Container.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatal("CPU limit added")
	}
	r.Source = []byte(resourceBoundsSource("      resources:\n        requests: {cpu: 5, memory: 128Mi}\n"))
	if _, e := Build(config, r); e == nil || !strings.Contains(e.Error(), "administrator L ceiling 4") {
		t.Fatalf("pipeline annotation raised grant: %v", e)
	}
	r.ResourceMaximum = periapsis.XL
	if _, e := Build(config, r); e != nil {
		t.Fatal(e)
	}
	r.Source = []byte(resourceBoundsSource(""))
	wf, e = Build(config, r)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(wf.Spec.Templates[0].Container.Resources, corev1.ResourceRequirements{}) {
		t.Fatal("undeclared resources changed")
	}
}
func TestResourceBoundsPriorityAndPodPatchesCannotBypass(t *testing.T) {
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	for _, shape := range []string{"workflow-priority", "template-priority", "defaults-priority", "workflow-patch", "template-patch", "defaults-patch"} {
		t.Run(shape, func(t *testing.T) {
			r := resourceBoundsRequest(resourceBoundsSource(""))
			wf, e := argoworkflow.Decode(r.Source)
			if e != nil {
				t.Fatal(e)
			}
			switch shape {
			case "workflow-priority":
				wf.Spec.PodPriorityClassName = "repo-01"
			case "template-priority":
				wf.Spec.Templates[0].PriorityClassName = "repo-01"
			case "defaults-priority":
				wf.Spec.TemplateDefaults = wf.Spec.Templates[0].DeepCopy()
				wf.Spec.TemplateDefaults.PriorityClassName = "repo-01"
			case "workflow-patch":
				wf.Spec.PodSpecPatch = `{"containers":[{"name":"main","resources":{"requests":{"cpu":"8"}}}]}`
			case "template-patch":
				wf.Spec.Templates[0].PodSpecPatch = `{"priorityClassName":"repo-01"}`
			case "defaults-patch":
				wf.Spec.TemplateDefaults = wf.Spec.Templates[0].DeepCopy()
				wf.Spec.TemplateDefaults.PodSpecPatch = `{"priorityClassName":"repo-01"}`
			}
			r.Source, e = yaml.Marshal(wf)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = Build(config, r); e == nil {
				t.Fatal("priority/patch bypass")
			}
		})
	}
}
