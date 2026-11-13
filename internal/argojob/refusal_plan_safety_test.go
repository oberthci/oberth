package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// Row 113 remains an Oberth materialization invariant: it prevents adding an
// empty volume to the Pod patch when a containerSet mount has no declaration.
func TestRefusalContainerSetUnknownVolume(t *testing.T) {
	template := &wfv1.Template{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", VolumeMounts: []corev1.VolumeMount{{Name: "missing", MountPath: "/data"}}}}}}}
	volumes, err := containerSetPodVolumes(&wfv1.Workflow{}, template)
	if err == nil || !strings.Contains(err.Error(), "unknown volume") || len(volumes) != 0 {
		t.Fatalf("missing declaration yielded a Pod-patch volume: %+v %v", volumes, err)
	}
}
