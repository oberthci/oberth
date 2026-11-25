package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	yaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type pipelineResource struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string
	Metadata   struct{ Name, Namespace string }
	Spec       struct {
		VolumeName string `yaml:"volumeName"`
	}
	Data map[string]string
}

func pipelineResources(t *testing.T, path string) map[string]pipelineResource {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]pipelineResource{}
	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var object pipelineResource
		if err = decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		}
		key := object.Kind + "/" + object.Metadata.Name
		if err != nil || object.APIVersion != "v1" || object.Metadata.Name == "" {
			t.Fatal("invalid pipeline object", key, err)
		}
		if _, exists := objects[key]; exists {
			t.Fatal("duplicate pipeline object", key)
		}
		objects[key] = object
	}
	return objects
}

const (
	buildResourcesFile = "../../.oberth/pipeline-resources.yaml"
	walkResourcesFile  = "../../.oberth/walk-vm-resources.yaml"
)

// The build's file holds exactly what build and release read; the walk's
// objects live in their own file beside the walk.
func TestPipelineObjectsSupplyWorkflowReferences(t *testing.T) {
	objects := pipelineResources(t, buildResourcesFile)
	used := map[string]bool{}
	for _, name := range []string{"build", "release"} {
		workflow, err := argoworkflow.Decode(ownWorkflow(t, name))
		if err != nil {
			t.Fatal(err)
		}
		assertWorkflowObjects(t, objects, workflow, used)
	}
	for key := range objects {
		if !used[key] {
			t.Fatal("pipeline resources hold an object the build and release never read", key)
		}
	}
	walk, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	walkObjects := pipelineResources(t, walkResourcesFile)
	for key := range walkObjects {
		if _, shared := objects[key]; shared {
			t.Fatal("walk object duplicated in the build's resources", key)
		}
	}
	assertWorkflowObjects(t, objects, walk, map[string]bool{})
	assertWalkObjects(t, walkObjects, walk)
}

func assertWorkflowObjects(t *testing.T, objects map[string]pipelineResource, workflow *wfv1.Workflow, used map[string]bool) {
	t.Helper()
	assertCacheObjects(t, objects, workflow.Spec.Volumes, used)
	for _, template := range workflow.Spec.Templates {
		assertCacheObjects(t, objects, template.Volumes, used)
		assertConfigObjects(t, objects, template, used)
	}
}

func assertCacheObjects(t *testing.T, objects map[string]pipelineResource, volumes []corev1.Volume, used map[string]bool) {
	t.Helper()
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		key := "PersistentVolumeClaim/" + volume.PersistentVolumeClaim.ClaimName
		if object := objects[key]; object.Metadata.Namespace != "REPLACE_WITH_PIPELINE_NAMESPACE" {
			t.Fatal("workflow cache has no pipeline claim", key)
		}
		used[key] = true
	}
}

func assertConfigObjects(t *testing.T, objects map[string]pipelineResource, template wfv1.Template, used map[string]bool) {
	t.Helper()
	if template.Synchronization != nil {
		for _, semaphore := range template.Synchronization.Semaphores {
			if key := semaphore.ConfigMapKeyRef; key != nil {
				assertConfigKey(t, objects, key.Name, key.Key)
				used["ConfigMap/"+key.Name] = true
			}
		}
	}
	if template.Container != nil {
		for _, env := range template.Container.Env {
			if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				key := env.ValueFrom.ConfigMapKeyRef
				assertConfigKey(t, objects, key.Name, key.Key)
				used["ConfigMap/"+key.Name] = true
			}
		}
	}
}

func assertConfigKey(t *testing.T, objects map[string]pipelineResource, name, key string) {
	t.Helper()
	object := objects["ConfigMap/"+name]
	if object.Metadata.Namespace != "REPLACE_WITH_PIPELINE_NAMESPACE" || object.Data[key] == "" {
		t.Fatal("workflow ConfigMap input has no object/key", name, key)
	}
}

func assertWalkObjects(t *testing.T, objects map[string]pipelineResource, workflow *wfv1.Workflow) {
	t.Helper()
	var manifest struct {
		Metadata struct{ Namespace string }
		Spec     struct {
			Volumes []struct {
				Ephemeral struct {
					PersistentVolumeClaim struct {
						ClaimName string `yaml:"claimName"`
					} `yaml:"persistentVolumeClaim"`
				}
			}
		}
	}
	if err := yaml.Unmarshal([]byte(workflow.GetTemplateByName("vm").Resource.Manifest), &manifest); err != nil {
		t.Fatal(err)
	}
	namespace := manifest.Metadata.Namespace
	if objects["Namespace/"+namespace].Metadata.Name != namespace {
		t.Fatal("walk namespace is not provided", namespace)
	}
	claim := manifest.Spec.Volumes[0].Ephemeral.PersistentVolumeClaim.ClaimName
	object := objects["PersistentVolumeClaim/"+claim]
	if claim == "" || object.Metadata.Namespace != namespace || object.Spec.VolumeName == "" {
		t.Fatal("walk claim must bind a separately prepared guest volume", claim)
	}
}
