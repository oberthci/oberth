package argoworkflow

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestRefusalSupportedWorkflowAnnotations(t *testing.T) {
	for key, value := range map[string]string{SecretPathsAnnotation: "", SourcesAnnotation: "[]", GoProxyAnnotation: GoProxyMirrorOnly, NonrootTemplatesAnnotation: "main", "example.test/note": "author"} {
		t.Run(key, func(t *testing.T) {
			w, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			w.Annotations = map[string]string{key: value}
			before := w.DeepCopy()
			if err := Admit(w, Policy{}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(w, before) {
				t.Fatal("admission mutated author metadata")
			}
		})
	}
	for _, key := range []string{"oberth.ci/assist", "oberth.ci/assist-hold", WorkspaceMountsAnnotation} {
		t.Run(key, func(t *testing.T) {
			w, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			w.Annotations = map[string]string{key: "true"}
			if err := Admit(w, Policy{}); err == nil {
				t.Fatal("unsupported Workflow-level ownership key admitted")
			}
		})
	}
}

func TestRefusalNativeEnvAcrossTemplates(t *testing.T) {
	for _, kind := range []string{"container", "script", "sidecar", "init", "containerSet", "inline", "defaults"} {
		t.Run(kind, func(t *testing.T) {
			w, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			c := w.Spec.Templates[0].Container.DeepCopy()
			c.Env = []corev1.EnvVar{
				{Name: "CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
				{Name: "IDENTITY", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}},
			}
			leaf := &w.Spec.Templates[0]
			switch kind {
			case "container":
				leaf.Container = c
			case "script":
				leaf.Container = nil
				leaf.Script = &wfv1.ScriptTemplate{Container: *c, Source: "true"}
			case "sidecar":
				c.Name = "side"
				leaf.Sidecars = []wfv1.UserContainer{{Container: *c}}
			case "init":
				c.Name = "setup"
				leaf.InitContainers = []wfv1.UserContainer{{Container: *c}}
			case "containerSet":
				c.Name = "main"
				leaf.Container = nil
				leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *c}}}
			case "inline":
				w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "inline", Inline: &wfv1.Template{Container: c}}}}}}
			case "defaults":
				w.Spec.TemplateDefaults = &wfv1.Template{Container: c}
			}
			if err := Admit(w, Policy{}); err != nil {
				t.Fatal(err)
			}
			c.Env[1].ValueFrom.FieldRef.FieldPath = "spec.nodeName"
			// All installed template forms retain the same FieldRef pointer.
			if err := Admit(w, Policy{}); err == nil {
				t.Fatal("node fieldRef admitted")
			}
		})
	}
}
