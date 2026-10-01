package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func seededRequest(t *testing.T, source string) Request {
	t.Helper()
	request := testRequest(periapsis.TriggerCI, source)
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
	if artifacts.Name == source.Name {
		t.Fatal("artifacts and source share a volume entry, so both inherit the same readOnly claim reference")
	}
}

func TestTheArtifactsVolumeReferencesTheRunClaimWritable(t *testing.T) {
	workflow, err := Build(testConfig(), seededRequest(t, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var artifacts, source *corev1.Volume
	for index := range workflow.Spec.Volumes {
		volume := &workflow.Spec.Volumes[index]
		switch volume.Name {
		case ArtifactsVolumeName:
			artifacts = volume
		case SourceVolumeName:
			source = volume
		}
	}
	if artifacts == nil || artifacts.PersistentVolumeClaim == nil {
		t.Fatal("no artifacts volume backed by the run claim")
	}
	if artifacts.PersistentVolumeClaim.ReadOnly {
		t.Fatal("the artifacts volume source is read-only, which makes the mount read-only whatever the mount says")
	}
	if source == nil || source.PersistentVolumeClaim == nil || !source.PersistentVolumeClaim.ReadOnly {
		t.Fatal("the source volume must stay a read-only claim reference")
	}
	if artifacts.PersistentVolumeClaim.ClaimName != source.PersistentVolumeClaim.ClaimName {
		t.Fatal("artifacts should ride the run's existing claim, not a second one")
	}
}

func TestARunWithNoSeededArtifactsDirectoryGetsNoMount(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyDocument)
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
	for _, variable := range container.Env {
		if variable.Name == "OBERTH_ARTIFACTS" {
			t.Fatal("OBERTH_ARTIFACTS points at a directory that was never created")
		}
	}
}

func TestAPipelineWithoutASourceVolumeIsUnchanged(t *testing.T) {
	before, err := Build(testConfig(), testRequest(periapsis.TriggerCI, greedyDocument))
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

func TestArtifactsAddNothingBesidesTheirOwnVolumeMountAndVariable(t *testing.T) {
	plain := testRequest(periapsis.TriggerCI, greedyDocument)
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
	var volumes []corev1.Volume
	for _, volume := range stripped.Spec.Volumes {
		if volume.Name != ArtifactsVolumeName {
			volumes = append(volumes, volume)
		}
	}
	stripped.Spec.Volumes = volumes
	for index := range stripped.Spec.Templates {
		container := stripped.Spec.Templates[index].Container
		if container == nil {
			continue
		}
		var mounts []corev1.VolumeMount
		for _, mount := range container.VolumeMounts {
			if mount.Name != ArtifactsVolumeName {
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
		t.Fatalf("artifacts changed something other than their own volume, mount and variable:\nwith    %s\nwithout %s",
			renderJSON(t, stripped), renderJSON(t, before))
	}
}
