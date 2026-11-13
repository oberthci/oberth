package argojob

import (
	"encoding/json"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestNativeExecutorResourcesStayOutOfTemplatePatches(t *testing.T) {
	logs := true
	for _, leaf := range []wfv1.Template{
		{Name: "container", Container: &corev1.Container{}},
		{Name: "script", Script: &wfv1.ScriptTemplate{}},
		{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{}},
		{Name: "resource", Resource: &wfv1.ResourceTemplate{Action: "create"}},
		{Name: "resource-logs", Resource: &wfv1.ResourceTemplate{Action: "create"}, ArchiveLocation: &wfv1.ArtifactLocation{ArchiveLogs: &logs}},
		{Name: "data", Data: &wfv1.Data{}},
	} {
		t.Run(leaf.Name, func(t *testing.T) {
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{helperStorageAnnotation: helperStoragePolicy}}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf}}}
			if err := projectExecutorTokens(wf, testConfig()); err != nil {
				t.Fatal(err)
			}
			assertNativeExecutorPatch(t, wf.Spec.Templates[0].PodSpecPatch)
		})
	}
}

func assertNativeExecutorPatch(t *testing.T, raw string) {
	t.Helper()
	var patch map[string]any
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatal(err)
	}
	foundWaiter := false
	for _, group := range []string{"initContainers", "containers"} {
		containers, _ := patch[group].([]any)
		for _, item := range containers {
			c := item.(map[string]any)
			if c["name"] == executorTokenWaiter {
				foundWaiter = true
				if _, ok := c["resources"]; !ok {
					t.Fatal("Oberth token waiter lost its budget")
				}
			}
			if c["name"] == "init" || c["name"] == "wait" || c["name"] == "main" {
				if _, ok := c["resources"]; ok {
					t.Fatalf("Argo %v resources are still patched", c["name"])
				}
			}
		}
	}
	if !foundWaiter {
		t.Fatal("token waiter missing")
	}
}

func TestCompiledWorkflowKeepsAuthoredResourcesWithNativeExecutors(t *testing.T) {
	source := resourceBoundsSource("      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi, ephemeral-storage: 128Mi}\n")
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	request := resourceBoundsRequest(source)
	before, err := argoworkflow.Decode(request.Source)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wf.Spec.Templates[0].Container.Resources, before.Spec.Templates[0].Container.Resources) {
		t.Fatal("authored resources changed")
	}
	assertNativeExecutorPatch(t, wf.Spec.Templates[0].PodSpecPatch)
}

// Model an operator's explicit native resources before the server's patch. This
// fixture also protects repository main-storage budgets under a bounded executor.
func nativeBudgetControllerShape(pod corev1.PodSpec) corev1.PodSpec {
	for i := range pod.Containers {
		if pod.Containers[i].Name == "wait" {
			pod.Containers[i].Resources = argoworkflow.ExecutorResources()
		}
	}
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == "init" {
			pod.InitContainers[i].Resources = argoworkflow.ExecutorResources()
		}
	}
	return pod
}

func TestTokenWaiterKeepsCurrentStoragePolicy(t *testing.T) {
	for _, declared := range []bool{false, true} {
		resources := "      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi}\n"
		if declared {
			resources = "      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi, ephemeral-storage: 128Mi}\n"
		}
		config := testConfig()
		config.RunnerImagePrefixes = []string{"example.invalid/"}
		wf, err := Build(config, resourceBoundsRequest(resourceBoundsSource(resources)))
		if err != nil {
			t.Fatal(err)
		}
		var patch corev1.PodSpec
		if err := json.Unmarshal([]byte(wf.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, container := range patch.InitContainers {
			if container.Name == executorTokenWaiter {
				found = true
				assertControlContainer(t, container, declared)
			}
		}
		if !found {
			t.Fatal("Oberth token waiter missing")
		}
	}
}

func TestWorkflowPatchPreservesNativeOperatorExecutorResources(t *testing.T) {
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	wf, err := Build(config, resourceBoundsRequest(resourceBoundsSource("")))
	if err != nil {
		t.Fatal(err)
	}
	main := *wf.Spec.Templates[0].Container.DeepCopy()
	before := nativeBudgetControllerShape(credentialWaitControllerShape(main))
	for i := range before.Containers {
		if before.Containers[i].Name == "wait" {
			before.Containers[i].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")}
		}
	}
	after := applyCredentialWaitPatch(t, before, wf.Spec.Templates[0].PodSpecPatch)
	for _, expected := range append(before.InitContainers, before.Containers...) {
		for _, actual := range append(after.InitContainers, after.Containers...) {
			if expected.Name == actual.Name && !reflect.DeepEqual(expected.Resources, actual.Resources) {
				t.Fatalf("%s native resources were rewritten", expected.Name)
			}
		}
	}
}

func TestNativeExecutorDefaultsLeaveUndeclaredStepsUncapped(t *testing.T) {
	resources := "      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi}\n"
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"example.invalid/"}
	wf, err := Build(cfg, resourceBoundsRequest(resourceBoundsSource(resources)))
	if err != nil {
		t.Fatal(err)
	}
	main := *wf.Spec.Templates[0].Container.DeepCopy()
	main.Name = "main"
	before := nativeBudgetControllerShape(credentialWaitControllerShape(main))
	after := applyCredentialWaitPatch(t, before, wf.Spec.Templates[0].PodSpecPatch)
	for _, container := range append(after.InitContainers, after.Containers...) {
		if container.Name != "init" && container.Name != "wait" {
			continue
		}
		if len(container.Resources.Requests) != 2 || container.Resources.Requests.Cpu().MilliValue() != 50 || container.Resources.Requests.Memory().Value() != 64<<20 || len(container.Resources.Limits) != 0 {
			t.Fatalf("native executor %s has more than CPU/memory requests: %+v", container.Name, container.Resources)
		}
	}
	if got := podStorageLimit(after); got != 0 {
		t.Fatalf("native defaults created a Pod storage cap of %d", got)
	}
}
