package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func assertBoundedControlContainer(t *testing.T, container corev1.Container) {
	t.Helper()
	for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request := container.Resources.Requests[key]
		limit := container.Resources.Limits[key]
		if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			t.Errorf("helper %s requires positive ordered %s requests and limits", container.Name, key)
		}
	}
	if container.Resources.Limits.Cpu().MilliValue() > 4000 || container.Resources.Limits.Memory().Value() > 8<<30 {
		t.Errorf("helper %s exceeds the container ceiling", container.Name)
	}
}

func TestInjectedControlBudgetsCoverUnpinnedExecutorContainers(t *testing.T) {
	archiveLogs := true
	cases := []struct {
		name     string
		leaf     wfv1.Template
		wantWait bool
		wantMain bool
	}{
		{name: "container", leaf: wfv1.Template{Container: &corev1.Container{}}, wantWait: true},
		{name: "resource", leaf: wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create"}}, wantMain: true},
		{name: "resource-logs", leaf: wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create"}, ArchiveLocation: &wfv1.ArtifactLocation{ArchiveLogs: &archiveLogs}}, wantWait: true, wantMain: true},
		{name: "data", leaf: wfv1.Template{Data: &wfv1.Data{}}, wantMain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := tc.leaf
			leaf.Name = "step"
			workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf}}}
			if err := projectExecutorTokens(workflow, testConfig()); err != nil {
				t.Fatal(err)
			}
			var patch corev1.PodSpec
			if err := json.Unmarshal([]byte(workflow.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, container := range append(patch.InitContainers, patch.Containers...) {
				assertBoundedControlContainer(t, container)
				if seen[container.Name] {
					t.Errorf("duplicate helper %s", container.Name)
				}
				seen[container.Name] = true
			}
			for _, name := range []string{executorTokenWaiter, "init"} {
				if !seen[name] {
					t.Errorf("missing explicit %s budget", name)
				}
			}
			if seen["wait"] != tc.wantWait || seen["main"] != tc.wantMain {
				t.Errorf("executor shape has wait=%t main=%t, want wait=%t main=%t", seen["wait"], seen["main"], tc.wantWait, tc.wantMain)
			}
		})
	}
}
