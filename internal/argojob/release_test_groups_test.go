package argojob

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Release tests must have the same disjoint package partition, watchdogs and
// resource envelope as branch CI. A single all-package Pod defeats isolation.
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
		if !reflect.DeepEqual(got.ActiveDeadlineSeconds, want.ActiveDeadlineSeconds) || !reflect.DeepEqual(got.Container.Resources, want.Container.Resources) {
			t.Errorf("%s must retain branch watchdog and resource profile", task.Name)
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
