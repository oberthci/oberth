package argojob

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestPinnedSourcesPublicRetryAndAdoption(t *testing.T) {
	for _, profile := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "profile"}[profile], func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			document := greedyDocument
			if profile {
				pod, cm := nonrootVerifierObjects()
				kube = fake.NewClientset(pod, cm)
				config.NonrootProfile = argoworkflow.NonrootStaticProfile
				document = nonrootTestDocument
			}
			runningSeedPods(kube)
			client := &createRetryWorkflowClient{}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var seeds []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
			source := pipeline.PinnedSource{Repository: "forge/acme/producer", Commit: strings.Repeat("a", 40), Path: "/work/sources/producer"}
			body, _ := json.Marshal([]pipeline.PinnedSource{source})
			request := testRequest(pipeline.TriggerCI, "sources: "+string(body)+"\n"+document)
			request.SourceDir = writeCheckout(t)
			request.PinnedSources = []PinnedSourceTree{{Source: source, Directory: t.TempDir()}}
			if profile {
				request, err = controller.PrepareNonroot(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			original := client.stored.DeepCopy()
			seedCount := len(seeds)
			adoption := AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch, PinnedSources: []pipeline.PinnedSource{source}}
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal("valid retry", err)
			}
			if _, found, err := controller.Adopt(t.Context(), adoption); err != nil || !found {
				t.Fatal("valid adoption", found, err)
			}
			for _, mode := range []string{"writable", "alias", "defaults-alias", "set-alias", "template-alias", "defaults-template-alias", "foreign", "foreign-source"} {
				t.Run(mode, func(t *testing.T) {
					client.stored = original.DeepCopy()
					live := client.stored
					mount := corev1.VolumeMount{Name: "alias", MountPath: "/writable", SubPath: "pinned-sources"}
					volume := corev1.Volume{Name: "alias", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sourceClaimName(live.Name)}}}
					live.Spec.Volumes = append(live.Spec.Volumes, volume)
					switch mode {
					case "writable", "foreign", "foreign-source":
						for i := range live.Spec.Templates[0].Container.VolumeMounts {
							m := &live.Spec.Templates[0].Container.VolumeMounts[i]
							if m.MountPath == source.Path {
								m.ReadOnly = false
							}
						}
						if mode == "foreign" {
							live.Annotations[runIDAnnotation] = "foreign-run"
						}
						if mode == "foreign-source" {
							live.Annotations[identityAnnotation] = "foreign-source"
						}
					case "alias":
						live.Spec.Templates[0].Container.VolumeMounts = append(live.Spec.Templates[0].Container.VolumeMounts, mount)
					case "defaults-alias":
						live.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{VolumeMounts: []corev1.VolumeMount{mount}}}
					case "set-alias":
						live.Spec.Templates = append(live.Spec.Templates, wfv1.Template{Name: "alias-set", ContainerSet: &wfv1.ContainerSetTemplate{VolumeMounts: []corev1.VolumeMount{mount}}})
					case "defaults-template-alias":
						live.Spec.Volumes = live.Spec.Volumes[:len(live.Spec.Volumes)-1]
						live.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{VolumeMounts: []corev1.VolumeMount{mount}}}
						live.Spec.Templates[0].Volumes = append(live.Spec.Templates[0].Volumes, volume)
					case "template-alias":
						live.Spec.Volumes = live.Spec.Volumes[:len(live.Spec.Volumes)-1]
						live.Spec.Templates[0].Volumes = append(live.Spec.Templates[0].Volumes, volume)
						live.Spec.Templates[0].Container.VolumeMounts = append(live.Spec.Templates[0].Container.VolumeMounts, mount)
					}
					before := live.DeepCopy()
					_, err := controller.Create(t.Context(), request)
					var mismatch *workflowSpecMismatch
					if err == nil || errors.As(err, &mismatch) != (!strings.HasPrefix(mode, "foreign")) {
						t.Errorf("retry classification: %T %v", err, err)
					}
					if mismatch != nil && (!mismatch.AmbiguousCreate() || !mismatch.WorkflowSpecMismatch()) {
						t.Fatal("lost cleanup classification")
					}
					if _, found, err := controller.Adopt(t.Context(), adoption); !errors.Is(err, ErrAdoptionRefused) || found {
						t.Errorf("adoption accepted: %v %v", found, err)
					}
					if len(seeds) != seedCount || len(client.posts) != 1 || !reflect.DeepEqual(before, client.stored) {
						t.Fatal("retry/adoption mutated delivery")
					}
				})
			}
		})
	}
}
