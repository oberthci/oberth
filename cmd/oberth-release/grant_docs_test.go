package main

import (
	"os"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
)

func TestPipelineGrantDocsCoverWorkflowReferences(t *testing.T) {
	body, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := normalizedGrantText(string(body))
	for _, name := range []string{"build", "release"} {
		wf, err := argoworkflow.Decode(ownWorkflow(t, name))
		if err != nil {
			t.Fatal(err)
		}
		tier := "ci"
		if name == "release" {
			tier = "release"
		}
		assertVolumeGrantDocs(t, doc, wf.Spec.Volumes, tier)
		for _, template := range wf.Spec.Templates {
			assertVolumeGrantDocs(t, doc, template.Volumes, tier)
			assertStepGrantDocs(t, doc, wf, template)
		}
	}
}

func normalizedGrantText(text string) string {
	text = strings.ReplaceAll(text, "\\\n", " ")
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "'", "")), " ")
}

func assertGrantDocumented(t *testing.T, doc, command string) {
	t.Helper()
	if !strings.Contains(doc, command) {
		t.Fatal("releasing.md has no matching workflow grant command", command)
	}
}

func assertVolumeGrantDocs(t *testing.T, doc string, volumes []corev1.Volume, tier string) {
	t.Helper()
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil {
			assertGrantDocumented(t, doc, "oberth access allow --kind pvc --tier "+tier+" <repo> "+volume.PersistentVolumeClaim.ClaimName)
		}
	}
}

func assertStepGrantDocs(t *testing.T, doc string, workflow *wfv1.Workflow, template wfv1.Template) {
	t.Helper()
	file := ".oberth/" + workflow.Spec.Entrypoint + ".yaml"
	key := " <repo> " + file + " " + template.Name + " "
	assertTokenGrantDocs(t, doc, key, template.Volumes)
	container := template.Container
	if container == nil {
		return
	}
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
			assertGrantDocumented(t, doc, "oberth access allow --kind configmap"+key+env.ValueFrom.ConfigMapKeyRef.Name)
		}
	}
	for _, path := range strings.Split(template.Metadata.Annotations[argoworkflow.SecretPathsAnnotation], ",") {
		if path = strings.TrimSpace(path); path != "" {
			assertGrantDocumented(t, doc, "oberth secret put --repo <repo> --file "+file+" --step "+template.Name+" "+path+" < <")
		}
	}
}

func assertTokenGrantDocs(t *testing.T, doc, key string, volumes []corev1.Volume) {
	t.Helper()
	for _, volume := range volumes {
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					assertGrantDocumented(t, doc, "oberth access allow --kind identity --audience "+source.ServiceAccountToken.Audience+key+"identity")
				}
			}
		}
	}
}
