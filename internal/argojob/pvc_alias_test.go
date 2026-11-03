package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// Count effective Pod volumes after the server's strategic patch. ContainerSet
// patches repeat workflow volumes by name; those are replacements, not aliases.
func TestBuildOneVolumePerPVCPerPod(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerset", "sidecars", "inline-dag", "inline-steps", "none"} {
		t.Run(shape, func(t *testing.T) {
			request := isolationControllerRequest(t, shape)
			request.SourceVolume.ArtifactsSubPath = artifactsSubPath
			workflow, err := Build(testConfig(), request)
			if err != nil {
				t.Fatal(err)
			}
			pods := 0
			walkTemplates(workflow, func(leaf *wfv1.Template) {
				if templateMainContainer(leaf) == nil {
					return
				}
				pods++
				pod := corev1.Pod{Spec: corev1.PodSpec{Volumes: append(append([]corev1.Volume{}, workflow.Spec.Volumes...), leaf.Volumes...)}}
				raw, err := json.Marshal(pod.Spec)
				if err != nil {
					t.Fatal(err)
				}
				if leaf.PodSpecPatch != "" {
					raw, err = strategicpatch.StrategicMergePatch(raw, []byte(leaf.PodSpecPatch), corev1.PodSpec{})
					if err != nil {
						t.Fatal(err)
					}
				}
				pod.Spec = corev1.PodSpec{}
				if err := json.Unmarshal(raw, &pod.Spec); err != nil {
					t.Fatal(err)
				}
				claims := map[string][]string{}
				for _, volume := range pod.Spec.Volumes {
					if pvc := volume.PersistentVolumeClaim; pvc != nil {
						claims[pvc.ClaimName] = append(claims[pvc.ClaimName], volume.Name)
					}
				}
				if got := claims[request.SourceVolume.ClaimName]; len(got) != 1 {
					t.Errorf("template %q: one PVC must yield exactly one volume entry per Pod; got %v", leaf.Name, got)
				}
				for claim, names := range claims {
					if len(names) != 1 {
						t.Errorf("PVC %q has aliases %v", claim, names)
					}
				}
			})
			if pods == 0 {
				t.Fatal("no Pod leaf inspected")
			}
		})
	}
}

func TestSharedClaimArtifactMountPolicy(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerset", "sidecars", "inline-dag", "inline-steps"} {
		for _, mode := range []string{"", "none", "cache", "readonly"} {
			t.Run(shape+"/"+mode, func(t *testing.T) {
				request := isolationControllerRequest(t, shape)
				request.SourceVolume.ArtifactsSubPath = artifactsSubPath
				var authored wfv1.Workflow
				if err := json.Unmarshal(request.Source, &authored); err != nil {
					t.Fatal(err)
				}
				walkTemplates(&authored, func(leaf *wfv1.Template) {
					if templateMainContainer(leaf) != nil {
						if mode == "" {
							delete(leaf.Metadata.Annotations, "oberth.ci/workspace-mounts")
						} else {
							leaf.Metadata.Annotations["oberth.ci/workspace-mounts"] = mode
						}
					}
				})
				var err error
				request.Source, err = json.Marshal(authored)
				if err != nil {
					t.Fatal(err)
				}
				workflow, err := Build(testConfig(), request)
				if err != nil {
					t.Fatal(err)
				}
				containers := 0
				walkTemplates(workflow, func(leaf *wfv1.Template) {
					templateContainers(leaf, func(c *corev1.Container) {
						containers++
						source, artifacts := 0, 0
						for _, mount := range c.VolumeMounts {
							if mount.MountPath == ArtifactsMountPath {
								artifacts++
								if mount.Name != SourceVolumeName || mount.SubPath != artifactsSubPath || mount.ReadOnly != (mode == "readonly") {
									t.Errorf("%s artifact mount: %+v", c.Name, mount)
								}
							} else if mount.Name == SourceVolumeName {
								if !mount.ReadOnly {
									t.Errorf("%s input mount writable: %+v", c.Name, mount)
								}
								if mount.MountPath == SourceMountPath {
									source++
								}
							}
						}
						if source != 1 {
							t.Errorf("%s has %d source mounts", c.Name, source)
						}
						want := 1
						if mode == "none" || mode == "cache" {
							want = 0
						}
						if artifacts != want {
							t.Errorf("%s has %d artifact mounts, want %d", c.Name, artifacts, want)
						}
					})
				})
				if containers == 0 {
					t.Fatal("no containers inspected")
				}
			})
		}
	}
}
