package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func seededRequest(t *testing.T, source string) Request {
	t.Helper()
	request := testRequest(pipeline.TriggerCI, source)
	request.SourceVolume = SourceVolume{
		ClaimName:        "oberth-src-abc",
		SubPath:          "src",
		ArtifactsSubPath: artifactsSubPath,
	}
	return request
}

func containerOf(t *testing.T, workflow *wfv1.Workflow, name string) *corev1.Container {
	t.Helper()
	for index := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[index]
		if template.Name == name && template.Container != nil {
			return template.Container
		}
	}
	t.Fatalf("template %q has no container", name)
	return nil
}

func TestEveryStepLearnsWhereToPutArtifacts(t *testing.T) {
	workflow, err := Build(testConfig(), seededRequest(t, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	container := containerOf(t, workflow, "main")
	value := ""
	for _, variable := range container.Env {
		if variable.Name == "OBERTH_ARTIFACTS" {
			value = variable.Value
		}
	}
	if value == "" {
		t.Fatal("no OBERTH_ARTIFACTS in the step environment; a step cannot know where to write")
	}
	if value != ArtifactsMountPath {
		t.Fatalf("OBERTH_ARTIFACTS = %q, want the mount path %q", value, ArtifactsMountPath)
	}
}

func TestTheArtifactsMountIsWritableAndTheSourceMountIsNot(t *testing.T) {
	workflow, err := Build(testConfig(), seededRequest(t, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	container := containerOf(t, workflow, "main")

	var artifacts, source *corev1.VolumeMount
	for index := range container.VolumeMounts {
		mount := &container.VolumeMounts[index]
		switch mount.MountPath {
		case ArtifactsMountPath:
			artifacts = mount
		case SourceMountPath:
			source = mount
		}
	}
	if artifacts == nil {
		t.Fatal("no artifacts mount in the step container")
	}
	if artifacts.ReadOnly {
		t.Fatal("the artifacts mount is read-only, so no step could ever write an artifact")
	}
	if artifacts.SubPath != artifactsSubPath {
		t.Fatalf("artifacts mount subPath = %q, want %q", artifacts.SubPath, artifactsSubPath)
	}
	if source == nil || !source.ReadOnly {
		t.Fatal("the source mount must stay read-only")
	}
	if artifacts.Name != source.Name {
		t.Fatal("artifacts and source must share one volume with independent mount permissions")
	}
}

func TestTheArtifactsVolumeReferencesTheRunClaimWritable(t *testing.T) {
	workflow, err := Build(testConfig(), seededRequest(t, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var source *corev1.Volume
	claims := 0
	for index := range workflow.Spec.Volumes {
		volume := &workflow.Spec.Volumes[index]
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == "oberth-src-abc" {
			claims++
			source = volume
		}
	}
	if claims != 1 || source == nil || source.Name != SourceVolumeName || source.PersistentVolumeClaim.ReadOnly {
		t.Fatal("source and artifacts require exactly one writable claim reference; individual mounts protect inputs")
	}
}

func TestPodPVCMountPermissions(t *testing.T) {
	for _, shape := range []string{"container", "containerset"} {
		t.Run(shape, func(t *testing.T) {
			request := seededRequest(t, greedyDocument)
			if shape == "containerset" {
				request = isolationControllerRequest(t, shape)
				request.SourceVolume.ArtifactsSubPath = artifactsSubPath
			}
			workflow, err := Build(testConfig(), request)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			checked := 0
			mirrors := 0
			walkTemplates(workflow, func(template *wfv1.Template) {
				if template.Container == nil && template.Script == nil && template.ContainerSet == nil {
					return
				}
				templateContainers(template, func(container *corev1.Container) {
					for _, mount := range container.VolumeMounts {
						if mount.Name == SourceVolumeName && !isArtifactsMount(mount) && !mount.ReadOnly {
							t.Errorf("template %q container %q has a writable source mount at %q", template.Name, container.Name, mount.MountPath)
						}
					}
				})
				var patch struct {
					Volumes    []corev1.Volume `json:"volumes"`
					Containers []struct {
						Name         string               `json:"name"`
						VolumeMounts []corev1.VolumeMount `json:"volumeMounts"`
					} `json:"containers"`
				}
				if template.PodSpecPatch != "" {
					if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
						t.Fatalf("template %q Pod patch: %v", template.Name, err)
					}
				}
				for _, container := range patch.Containers {
					for _, mount := range container.VolumeMounts {
						if mount.Name == SourceVolumeName && !isArtifactsMount(mount) {
							mirrors++
							if !mount.ReadOnly {
								t.Errorf("template %q Pod patch leaves %q source mirror writable", template.Name, container.Name)
							}
						}
					}
				}
				claimNames := map[string]string{}
				volumes := append(append(append([]corev1.Volume(nil), workflow.Spec.Volumes...), template.Volumes...), patch.Volumes...)
				for _, volume := range volumes {
					claim := volume.PersistentVolumeClaim
					if claim == nil {
						continue
					}
					checked++
					if previous, seen := claimNames[claim.ClaimName]; seen && previous != volume.Name {
						t.Errorf("template %q mounts PVC %q as both %q and %q", template.Name, claim.ClaimName, previous, volume.Name)
					}
					claimNames[claim.ClaimName] = volume.Name
				}
			})
			if checked == 0 {
				t.Fatal("test did not inspect a Pod PVC reference")
			}
			if mirrors == 0 {
				t.Fatal("Pod patch did not protect an executor source mirror")
			}
		})
	}
}

func TestARunWithNoSeededArtifactsDirectoryGetsNoMount(t *testing.T) {
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	request.SourceVolume = SourceVolume{ClaimName: "oberth-src-abc", SubPath: "src"}
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, volume := range workflow.Spec.Volumes {
		if volume.Name == ArtifactsVolumeName {
			t.Fatal("an artifacts volume appeared for a run whose seeding never created the directory")
		}
	}
	container := containerOf(t, workflow, "main")
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == ArtifactsMountPath {
			t.Fatal("artifacts mount appeared without a seeded directory")
		}
	}
	for _, variable := range container.Env {
		if variable.Name == "OBERTH_ARTIFACTS" {
			t.Fatal("OBERTH_ARTIFACTS points at a directory that was never created")
		}
	}
}

func TestAPipelineWithoutASourceVolumeIsUnchanged(t *testing.T) {
	before, err := Build(testConfig(), testRequest(pipeline.TriggerCI, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, volume := range before.Spec.Volumes {
		if volume.Name == ArtifactsVolumeName {
			t.Fatal("artifacts volume injected with no claim to put it on")
		}
	}
	container := containerOf(t, before, "main")
	for _, variable := range container.Env {
		if strings.HasPrefix(variable.Name, "OBERTH_ARTIFACTS") {
			t.Fatalf("%s injected with no artifacts directory", variable.Name)
		}
	}
}

func TestArtifactsAddOnlyTheirMountAndVariable(t *testing.T) {
	plain := testRequest(pipeline.TriggerCI, greedyDocument)
	plain.SourceVolume = SourceVolume{ClaimName: "oberth-src-abc", SubPath: "src"}
	before, err := Build(testConfig(), plain)
	if err != nil {
		t.Fatalf("build without artifacts: %v", err)
	}
	after, err := Build(testConfig(), seededRequest(t, greedyDocument))
	if err != nil {
		t.Fatalf("build with artifacts: %v", err)
	}

	stripped := after.DeepCopy()
	for index := range stripped.Spec.Templates {
		container := stripped.Spec.Templates[index].Container
		if container == nil {
			continue
		}
		var mounts []corev1.VolumeMount
		for _, mount := range container.VolumeMounts {
			if !isArtifactsMount(mount) {
				mounts = append(mounts, mount)
			}
		}
		container.VolumeMounts = mounts
		var environment []corev1.EnvVar
		for _, variable := range container.Env {
			if variable.Name != "OBERTH_ARTIFACTS" {
				environment = append(environment, variable)
			}
		}
		container.Env = environment
	}

	withArtifacts := stripped.Annotations[identityAnnotation]
	withoutArtifacts := before.Annotations[identityAnnotation]
	if withArtifacts == withoutArtifacts {
		t.Fatal("the spec identity digest did not change, so it is not covering the pod spec")
	}
	delete(stripped.Annotations, identityAnnotation)
	delete(before.Annotations, identityAnnotation)

	if renderJSON(t, stripped) != renderJSON(t, before) {
		t.Fatalf("artifacts changed something other than their mount and variable:\nwith    %s\nwithout %s",
			renderJSON(t, stripped), renderJSON(t, before))
	}
}
