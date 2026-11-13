package argoworkflow

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A single position table exercises the prefix guard through every recursive
// carrier. Refusals must name the prefix/claim guard, so unrelated refusals
// (for example default helpers) cannot hide missing coverage.
func TestAdmitReservesOberthPrefixEverywhere(t *testing.T) {
	positions := []string{"container", "script", "containerSet-member", "initContainer", "sidecar", "resource-initContainer", "resource-sidecar", "workflow-volume", "template-volume", "volumeClaimTemplate", "container-mount", "script-mount", "containerSet-member-mount", "containerSet-common-mount", "initContainer-mount", "sidecar-mount", "volumeDevice"}
	for _, carrier := range []string{"direct", "defaults", "containerSet", "inline-dag", "inline-steps", "workflow-hook", "task-hook", "step-hook"} {
		for _, position := range positions {
			t.Run(carrier+"/"+position, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				image := wf.Spec.Templates[0].Container.Image
				leaf := executorAdmissionLeaf(t, wf, carrier)
				container := corev1.Container{Name: "helper", Image: image}
				name := "oberth-executor-token"
				if strings.HasSuffix(position, "-mount") || position == "volumeDevice" {
					container.VolumeMounts = []corev1.VolumeMount{{Name: "oberth-source", MountPath: "/scratch"}}
				}
				want := "reserved server-owned oberth- prefix"
				switch position {
				case "container":
					leaf.Container = &container
					leaf.Container.Name = name
				case "script", "script-mount":
					leaf.Script = &wfv1.ScriptTemplate{Container: container, Source: "true"}
					if position == "script" {
						leaf.Script.Name = name
					}
				case "containerSet-member", "containerSet-member-mount", "containerSet-common-mount":
					leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: container}}}
					if position == "containerSet-member" {
						leaf.ContainerSet.Containers[0].Name = name
					}
					if position == "containerSet-common-mount" {
						leaf.ContainerSet.VolumeMounts = container.VolumeMounts
						leaf.ContainerSet.Containers[0].VolumeMounts = nil
					}
				case "initContainer", "resource-initContainer", "initContainer-mount":
					if position != "initContainer-mount" {
						container.Name = name
					}
					leaf.InitContainers = []wfv1.UserContainer{{Container: container}}
				case "sidecar", "resource-sidecar", "sidecar-mount":
					if position != "sidecar-mount" {
						container.Name = name
					}
					leaf.Sidecars = []wfv1.UserContainer{{Container: container}}
				case "workflow-volume", "template-volume":
					volume := corev1.Volume{Name: "oberth-source", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}}
					if position == "workflow-volume" {
						wf.Spec.Volumes = []corev1.Volume{volume}
					} else {
						leaf.Volumes = []corev1.Volume{volume}
					}
				case "volumeClaimTemplate":
					wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "oberth-source"}}}
					want = "collides with a server-owned claim name"
				case "container-mount":
					leaf.Container = &container
				case "volumeDevice":
					container.VolumeMounts = nil
					container.VolumeDevices = []corev1.VolumeDevice{{Name: "oberth-source", DevicePath: "/dev/scratch"}}
					leaf.Container = &container
				}
				policy := Policy{}
				if strings.HasPrefix(position, "resource-") {
					resourceWorkflow, err := Decode([]byte(capabilityResourceDocument))
					if err != nil {
						t.Fatal(err)
					}
					leaf.Container, leaf.Script, leaf.ContainerSet = nil, nil, nil
					leaf.Resource = resourceWorkflow.Spec.Templates[0].Resource
					policy = capabilityPolicyForTest(t)
				}
				if err := Admit(wf, policy); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("server prefix admitted at %s: %v", position, err)
				}
			})
		}
	}
}

func TestEphemeralContainersAreNotExpressibleInWorkflows(t *testing.T) {
	wf := decodeAdmissible(t, nil)
	body, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	document["spec"].(map[string]any)["ephemeralContainers"] = []any{map[string]any{"name": "oberth-executor-token", "image": wf.Spec.Templates[0].Container.Image}}
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(body); err == nil || !strings.Contains(err.Error(), "ephemeralContainers") {
		t.Fatalf("unreviewable ephemeral container admitted: %v", err)
	}
}
