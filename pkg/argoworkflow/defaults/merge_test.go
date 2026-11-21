package defaults

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"reflect"
	"testing"
)

func TestMergeToNativeEdges(t *testing.T) {
	base := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Suspend: ptr.To(true), Hooks: wfv1.LifecycleHooks{"exit": {Template: "default"}, "running": {Template: "monitor"}}, WorkflowMetadata: &wfv1.WorkflowMetadata{LabelsFrom: map[string]wfv1.LabelValueFrom{"x": {Expression: "default"}, "y": {Expression: "extra"}}}}}
	target := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Suspend: ptr.To(false), Hooks: wfv1.LifecycleHooks{"exit": {Template: "authored"}}, WorkflowMetadata: &wfv1.WorkflowMetadata{LabelsFrom: map[string]wfv1.LabelValueFrom{"x": {Expression: "authored"}}}}}
	original := base.DeepCopy()
	if err := MergeTo(base, target); err != nil {
		t.Fatal(err)
	}
	if *target.Spec.Suspend || target.Spec.Hooks["exit"].Template != "authored" || target.Spec.Hooks["running"].Template != "monitor" || target.Spec.WorkflowMetadata.LabelsFrom["x"].Expression != "authored" || target.Spec.WorkflowMetadata.LabelsFrom["y"].Expression != "extra" {
		t.Fatalf("%+v", target.Spec)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatal("defaults changed")
	}
	if err := MergeTo(nil, target); err != nil {
		t.Fatal(err)
	}
	if err := MergeTo(base, nil); err != nil {
		t.Fatal(err)
	}
}
func TestEffectiveTemplateMergesHelpersAndIOOnce(t *testing.T) {
	base := &wfv1.Template{Sidecars: []wfv1.UserContainer{{}}, Inputs: wfv1.Inputs{Artifacts: []wfv1.Artifact{{Name: "source", Path: "/work/src"}}}}
	base.Sidecars[0].Name = "same"
	base.Sidecars[0].Image = "base"
	authored := &wfv1.Template{Sidecars: []wfv1.UserContainer{{}}}
	authored.Sidecars[0].Name = "same"
	authored.Sidecars[0].Image = "authored"
	merged, err := Template(base, authored)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Sidecars) != 1 || merged.Sidecars[0].Image != "authored" || len(merged.Inputs.Artifacts) != 1 {
		t.Fatal(merged)
	}
	merged.Inputs.Artifacts[0].Path = "changed"
	if base.Inputs.Artifacts[0].Path != "/work/src" {
		t.Fatal("input copy changed")
	}
}

func TestTemplateDefaultsPreserveContainerSetKind(t *testing.T) {
	authored := &wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "work", Image: "authored"}}}}}
	base := &wfv1.Template{Container: &corev1.Container{Image: "default"}}
	effective, err := Template(base, authored)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Container != nil || effective.ContainerSet == nil {
		t.Fatal("inapplicable container defaults widened the kind")
	}
}
