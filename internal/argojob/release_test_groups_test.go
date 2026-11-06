package argojob

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Release tests must have the same disjoint package partition, watchdogs and
// CPU envelope as branch CI. Release memory may grow above branch floors from
// retained release peaks; private caches need their own local storage bounds. A single all-package Pod defeats isolation.
func TestOberthReleaseRaceGroupsMatchBranch(t *testing.T) {
	branch, err := argoworkflow.Decode(loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	find := func(workflow *wfv1.Workflow, name string) wfv1.Template {
		t.Helper()
		for _, template := range workflow.Spec.Templates {
			if template.Name == name {
				return template
			}
		}
		t.Fatalf("missing template %s", name)
		return wfv1.Template{}
	}
	gate := find(release, "release-test")
	if gate.DAG == nil || gate.Container != nil || len(gate.DAG.Tasks) != 4 {
		t.Fatal("release-test must gate four isolated race groups")
	}
	pending := map[string]bool{"store": true, "service": true, "rest": true, "gitcache": true}
	// Release run 49213f5af2a95359986a00356ca1f3fb retained these Pod-local
	// sampled lower bounds (67/40/18 samples respectively). Use provisional
	// 1.5x headroom rounded up to Mi; these are not certified cold peaks.
	releaseLocalBytes := map[string]int64{"store": 2754265088, "service": 3591286784, "gitcache": 1838612480}
	for _, task := range gate.DAG.Tasks {
		if !pending[task.Name] || task.Template != "release-test-"+task.Name || task.Depends != "" || len(task.Dependencies) != 0 || task.ContinueOn != nil || task.When != "" {
			t.Fatalf("unexpected or bypassable race group: %+v", task)
		}
		delete(pending, task.Name)
		want := find(branch, "test-"+task.Name)
		got := find(release, task.Template)
		if got.Container == nil || len(got.Container.Args) < 3 {
			t.Fatalf("%s lost clean test bootstrap", task.Name)
		}
		command := append(append([]string{}, want.Container.Command...), want.Container.Args...)
		if !reflect.DeepEqual(got.Container.Args[2:], command) {
			t.Errorf("%s invocation = %v, want branch %v", task.Name, got.Container.Args[2:], command)
		}
		wantResources := want.Container.Resources.DeepCopy()
		if observed, ok := releaseLocalBytes[task.Name]; ok {
			const mib int64 = 1 << 20
			bound := ((observed*3 + 2*mib - 1) / (2 * mib)) * mib
			quantity := resource.MustParse(resource.NewQuantity(bound, resource.BinarySI).String())
			wantResources.Requests[corev1.ResourceEphemeralStorage] = quantity
			wantResources.Limits[corev1.ResourceEphemeralStorage] = quantity
		}
		if !reflect.DeepEqual(got.ActiveDeadlineSeconds, want.ActiveDeadlineSeconds) {
			t.Errorf("%s must retain its branch watchdog", task.Name)
		}
		for group, floor := range map[string]corev1.ResourceList{"requests": wantResources.Requests, "limits": wantResources.Limits} {
			actual := got.Container.Resources.Requests
			if group == "limits" {
				actual = got.Container.Resources.Limits
			}
			for name, minimum := range floor {
				quantity := actual[name]
				if quantity.Cmp(minimum) < 0 || (name == corev1.ResourceCPU && quantity.Cmp(minimum) != 0) {
					t.Errorf("%s %s %s lost its branch/release floor: %s < %s", task.Name, group, name, quantity.String(), minimum.String())
				}
			}
		}
		isolatedTools := false
		for _, mount := range got.Container.VolumeMounts {
			if mount.Name == "bootstrap-test-tools" {
				isolatedTools = mount.ReadOnly && mount.SubPath == task.Template
			}
			if mount.Name == "bootstrap-node-inputs" && task.Name != "rest" {
				t.Errorf("%s unnecessarily receives Node inputs", task.Name)
			}
		}
		if !isolatedTools {
			t.Errorf("%s must have its own read-only tools copy", task.Name)
		}
	}
}
