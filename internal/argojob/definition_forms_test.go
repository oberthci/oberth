package argojob_test

import (
	"context"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func definitionFormInput(w *wf.Workflow, kind string) argoworkflow.Policy {
	leaf := w.Spec.Templates[0]
	switch kind {
	case "container-set":
		container := *leaf.Container
		container.Name = "work"
		w.Spec.Templates[0] = wf.Template{Name: leaf.Name, ContainerSet: &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: container}}}}
	case "suspend":
		w.Spec.Templates[0] = wf.Template{Name: leaf.Name, Suspend: &wf.SuspendTemplate{Duration: "1"}}
	case "resource":
		w.Spec.Templates[0] = wf.Template{Name: leaf.Name, Resource: &wf.ResourceTemplate{Action: "create", Manifest: "apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: sample, namespace: resources}\nspec: {value: sample}\n"}}
		return argoworkflow.Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", Capability: &argoworkflow.Capability{Namespace: "resources", ServiceAccount: "resources", ResourceKinds: []argoworkflow.ResourceKind{{APIVersion: "example.test/v1", Kind: "Widget", Actions: []string{"create"}}}}}
	case "named-dag", "named-steps", "reference-dag", "reference-steps":
		return definitionCallInput(w, kind, leaf)
	}
	return argoworkflow.Policy{}
}

func definitionCallInput(w *wf.Workflow, kind string, leaf wf.Template) argoworkflow.Policy {
	name := leaf.Name
	var ref *wf.TemplateRef
	policy := argoworkflow.Policy{}
	if kind == "reference-dag" || kind == "reference-steps" {
		ref = &wf.TemplateRef{Name: "shared", Template: leaf.Name}
		name = ""
		object := &wf.WorkflowTemplate{ObjectMeta: meta.ObjectMeta{Name: "shared", Namespace: "ci", UID: "shared-uid", ResourceVersion: "1"}, Spec: wf.WorkflowSpec{Templates: []wf.Template{leaf}}}
		policy.TemplateGetter = func(context.Context, string) (*wf.WorkflowTemplate, error) { return object.DeepCopy(), nil }
	}
	root := wf.Template{Name: "group"}
	if kind == "named-dag" || kind == "reference-dag" {
		root.DAG = &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "work", Template: name, TemplateRef: ref}}}
	} else {
		root.Steps = []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "work", Template: name, TemplateRef: ref}}}}
	}
	w.Spec.Entrypoint = root.Name
	w.Spec.Templates = []wf.Template{root, leaf}
	return policy
}

func submittedInvocation(t *wf.Template) string {
	var name string
	var ref *wf.TemplateRef
	var inline *wf.Template
	switch t.GetType() {
	case wf.TemplateTypeDAG:
		call := t.DAG.Tasks[0]
		name, ref, inline = call.Template, call.TemplateRef, call.Inline
	case wf.TemplateTypeSteps:
		call := t.Steps[0].Steps[0]
		name, ref, inline = call.Template, call.TemplateRef, call.Inline
	default:
		return ""
	}
	if inline != nil {
		return "inline"
	}
	if ref != nil {
		return "reference"
	}
	if name != "" {
		return "named"
	}
	return ""
}

func requireAdmittedFormSamples(t *testing.T, templates map[wf.TemplateType]bool, calls map[string]bool) {
	t.Helper()
	for _, form := range argoworkflow.AdmittedTemplateForms() {
		if !templates[form] {
			t.Errorf("admitted template form %s has no submitted payload", form)
		}
	}
	for _, template := range []wf.TemplateType{wf.TemplateTypeDAG, wf.TemplateTypeSteps} {
		for _, invocation := range argoworkflow.AdmittedInvocationForms() {
			if !calls[string(template)+"/"+invocation] {
				t.Errorf("admitted invocation form %s/%s has no submitted payload", template, invocation)
			}
		}
	}
}
