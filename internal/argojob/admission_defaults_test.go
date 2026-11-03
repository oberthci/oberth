package argojob

import (
	"context"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestKubernetesPlacementDefaultsUndeclaredStepRequests(t *testing.T) {
	wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		if template.Name != "main" || template.Container == nil {
			continue
		}
		r := template.Container.Resources
		if r.Requests.Cpu().MilliValue() != 2000 || r.Requests.Memory().Value() != 2<<30 || len(r.Limits) != 0 {
			t.Fatalf("undeclared step has no conservative requests: %+v", r)
		}
		return
	}
	t.Fatal("main step missing")
}

func TestKubernetesPlacementDefaultsPreserveDeclarationsAndAllContainerShapes(t *testing.T) {
	declared := corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("125m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}
	before := declared.DeepCopy()
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
		{Name: "container", Container: &declared},
		{Name: "script", Script: &wfv1.ScriptTemplate{}},
		{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "member"}}}}},
		{Name: "helpers", InitContainers: []wfv1.UserContainer{{Container: corev1.Container{Name: "init"}}}, Sidecars: []wfv1.UserContainer{{Container: corev1.Container{Name: "sidecar"}}}},
		{Name: "inline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Inline: &wfv1.Template{Container: &corev1.Container{}}}}}},
	}}}
	applyRequestDefaults(wf, false)
	if !reflect.DeepEqual(declared, *before) {
		t.Fatal("explicit resources changed")
	}
	walkTemplates(wf, func(template *wfv1.Template) {
		check := func(c *corev1.Container) {
			if c != nil && (c.Resources.Requests.Cpu().Sign() <= 0 || c.Resources.Requests.Memory().Sign() <= 0) {
				t.Errorf("container lacks requests: %+v", c.Resources)
			}
		}
		check(template.Container)
		if template.Script != nil {
			check(&template.Script.Container)
		}
		if template.ContainerSet != nil {
			for i := range template.ContainerSet.Containers {
				check(&template.ContainerSet.Containers[i].Container)
			}
		}
		for i := range template.InitContainers {
			check(&template.InitContainers[i].Container)
		}
		for i := range template.Sidecars {
			check(&template.Sidecars[i].Container)
		}
	})
}

func TestKubernetesPlacementDefaultsHonorTemplateAndLimitOnlyResources(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		TemplateDefaults: &wfv1.Template{Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}},
		Templates:        []wfv1.Template{{Container: &corev1.Container{}}, {Script: &wfv1.ScriptTemplate{Container: corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}}}}}},
	}}
	applyRequestDefaults(wf, false)
	if r := wf.Spec.Templates[0].Container.Resources; r.Requests.Cpu().MilliValue() != 250 || r.Requests.Memory().Value() != 256<<20 {
		t.Fatal("template declarations lost", r)
	}
	if r := wf.Spec.Templates[1].Script.Resources; r.Requests.Cpu().MilliValue() != 100 || r.Requests.Memory().Value() != 128<<20 {
		t.Fatal("limits-only request semantics lost", r)
	}
}

func TestKubernetesPlacementRetryPreservesAlreadySubmittedLegacyRequests(t *testing.T) {
	config := seedTestConfig()
	kube := fake.NewClientset()
	runningSeedPods(kube)
	client := &createRetryWorkflowClient{}
	c, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	var seeds []seedExecCall
	c.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
	r := testRequest(pipeline.TriggerCI, greedyDocument)
	r.SourceDir = writeCheckout(t)
	r.omitRequestDefaults = true
	if _, err := c.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	before := client.stored.DeepCopy()
	seedCount := len(seeds)
	r.omitRequestDefaults = false
	if _, err := c.Create(context.Background(), r); err != nil {
		t.Fatal("upgrade retry changed the existing workflow", err)
	}
	if !reflect.DeepEqual(before.Spec, client.stored.Spec) || len(seeds) != seedCount || len(client.posts) != 1 {
		t.Fatal("legacy workflow or delivery footprint was rewritten")
	}
}
