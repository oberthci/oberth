package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/holdfs"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBuildProtectsHeldPodHelperFromAuthoredShadows(t *testing.T) {
	for _, target := range []string{holdfs.HelperPath, holdfs.HelperMountPath, "/run/oberth", "/run/oberth/assist/nested", "/run/oberth-other"} {
		t.Run(target, func(t *testing.T) {
			source, err := argoworkflow.Decode([]byte(assistDocument))
			if err != nil {
				t.Fatal(err)
			}
			for i := range source.Spec.Templates {
				if source.Spec.Templates[i].Container != nil {
					source.Spec.Templates[i].Container.VolumeMounts = append(source.Spec.Templates[i].Container.VolumeMounts, corev1.VolumeMount{Name: "work", MountPath: target, ReadOnly: true})
				}
			}
			encoded, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			wf, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, string(encoded)))
			if target != "/run/oberth-other" {
				var refused *argoworkflow.AdmissionError
				if !errors.As(err, &refused) || !strings.Contains(err.Error(), "server helper") {
					t.Fatalf("shadow reached held-Pod compilation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal("unrelated mount refused", err)
			}
			helpers := 0
			for _, template := range wf.Spec.Templates {
				if template.Container != nil && hasAssistBinary(template.Container) {
					helpers++
				}
			}
			if helpers == 0 || holdfs.HelperPath != holdfs.HelperMountPath+"/oberth" {
				t.Fatal("held Pods lost the executable used by the patch service")
			}
		})
	}
}

func TestExistingHeldPodShadowIsNotPatchCapable(t *testing.T) {
	for _, target := range []string{holdfs.HelperPath, "/run/oberth", "/var/run/oberth/assist/oberth", "/run/oberth-other"} {
		t.Run(target, func(t *testing.T) {
			pod := heldPodFixture("wf-test-1", "wf", "test", time.Minute, true)
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "authored-shadow", MountPath: target, ReadOnly: true})
			hold, ok := assistHoldOf(pod)
			if !ok || hold.PatchCapable != (target == "/run/oberth-other") {
				t.Fatalf("existing shadow trusted as helper: %+v", hold)
			}
			if target == "/run/oberth-other" {
				return
			}
			kube := fake.NewClientset(pod)
			controller, err := NewController(&staticWorkflowClient{}, kube, testConfig())
			if err != nil {
				t.Fatal(err)
			}
			var command []string
			controller.WithSourceSeeder(NewSourceSeeder(kube, func(_ context.Context, _, _, _ string, argv []string, _ io.Reader, _, _ io.Writer) error {
				command = append([]string(nil), argv...)
				return nil
			}, testConfig()))
			if err := controller.ReleaseAssistHold(t.Context(), pod.Name); err != nil {
				t.Fatal(err)
			}
			if len(command) != 2 || command[0] != "touch" || command[1] != AssistReleasePath {
				t.Fatal("release executed an untrusted helper", command)
			}
		})
	}
	pod := heldPodFixture("wf-test-1", "wf", "test", time.Minute, true)
	pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "block", DevicePath: holdfs.HelperPath}}
	if hold, _ := assistHoldOf(pod); hold.PatchCapable {
		t.Fatal("device-shadowed helper trusted")
	}
}
