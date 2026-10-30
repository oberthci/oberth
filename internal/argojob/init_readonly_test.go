package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestInitPopulatesLocalToolsBeforeReadonlyReaders(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerset", "inline-dag", "inline-steps"} {
		for _, reader := range []string{"main", "sidecar"} {
			t.Run(shape+"/"+reader, func(t *testing.T) {
				r := isolationRequest(t, func(w *wfv1.Workflow) {
					leaf := &w.Spec.Templates[0]
					leaf.AutomountServiceAccountToken = ptr.To(false)
					leaf.Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: "none"}
					leaf.Volumes = []corev1.Volume{{Name: "stock-cos-tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("32Mi"))}}}}
					main := leaf.Container
					main.VolumeMounts = []corev1.VolumeMount{{Name: "stock-cos-tools", MountPath: "/tools", SubPath: "bin", ReadOnly: reader == "main"}, {Name: "stock-cos-tools", MountPath: "/tools-alias"}}
					helper := corev1.Container{Name: "install-jq", Image: main.Image, Command: []string{"/bin/true"}, VolumeMounts: []corev1.VolumeMount{{Name: "stock-cos-tools", MountPath: "/tmp/stock-cos-tools"}}}
					leaf.InitContainers = []wfv1.UserContainer{{Container: helper}}
					helper = *helper.DeepCopy()
					helper.Name = "reader"
					helper.VolumeMounts[0].ReadOnly = reader == "sidecar"
					leaf.Sidecars = []wfv1.UserContainer{{Container: helper}}
					switch shape {
					case "script":
						leaf.Script = &wfv1.ScriptTemplate{Container: *main, Source: "true"}
						leaf.Container = nil
					case "containerset":
						main.Name = "main"
						leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *main}}}
						leaf.Container = nil
					case "inline-dag", "inline-steps":
						inline := leaf.DeepCopy()
						*leaf = wfv1.Template{Name: leaf.Name}
						if shape == "inline-dag" {
							leaf.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "test", Inline: inline}}}
						} else {
							leaf.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "test", Inline: inline}}}}
						}
					}
				})
				wf, err := Build(testConfig(), r)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				walkTemplates(wf, func(leaf *wfv1.Template) {
					if templateMainContainer(leaf) == nil {
						return
					}
					found = true
					templateContainers(leaf, func(c *corev1.Container) {
						for _, m := range c.VolumeMounts {
							if m.Name == "stock-cos-tools" && m.ReadOnly != (c.Name != "install-jq") {
								t.Errorf("%s tools mount readonly=%v: %+v", c.Name, m.ReadOnly, m)
							}
							if m.Name == SourceVolumeName && !m.ReadOnly {
								t.Errorf("%s source writable", c.Name)
							}
						}
					})
					if !strings.Contains(leaf.PodSpecPatch, `"/mainctrfs/tools"`) {
						t.Error("wait tools mirror lost readonly protection")
					}
				})
				if !found {
					t.Fatal("no leaf tested")
				}
			})
		}
	}
}

func TestInitReadonlyExceptionDoesNotCoverSharedOrConcurrentWriters(t *testing.T) {
	for _, kind := range []string{"workflow-emptydir", "template-pvc", "work", "source", "cache", "artifacts", "token", "secrets", "server-tmp", "restartable-init", "explicit-init-readonly"} {
		t.Run(kind, func(t *testing.T) {
			name := "tools"
			switch kind {
			case "work":
				name = WorkspaceVolumeName
			case "source":
				name = SourceVolumeName
			case "cache":
				name = CacheVolumeName
			case "artifacts":
				name = ArtifactsVolumeName
			case "token":
				name = ReleaseTokenVolumeName
			case "secrets":
				name = SecretsVolumeName
			case "server-tmp":
				name = stepTmpVolumeName
			}
			tmpl := &wfv1.Template{
				Container:      &corev1.Container{VolumeMounts: []corev1.VolumeMount{{Name: name, MountPath: "/read", SubPath: "bin", ReadOnly: true}}},
				InitContainers: []wfv1.UserContainer{{Container: corev1.Container{Name: "populate", VolumeMounts: []corev1.VolumeMount{{Name: name, MountPath: "/write"}}}}},
				Volumes:        []corev1.Volume{{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			}
			switch kind {
			case "workflow-emptydir":
				tmpl.Volumes = nil // Not declared at template scope.
			case "template-pvc":
				tmpl.Volumes[0].VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-tools"}}
			case "restartable-init":
				tmpl.InitContainers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
			case "explicit-init-readonly":
				tmpl.InitContainers[0].VolumeMounts = append(tmpl.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: "/own-read", SubPath: "bin", ReadOnly: true})
			}
			// Source/token/secret protection is injected independently of authored
			// readonlyMounts. Supply that existing policy at this unit boundary too.
			protected := []corev1.VolumeMount{tmpl.Container.VolumeMounts[0]}
			preserveWorkspaceReadOnly(tmpl, protected)
			if !tmpl.InitContainers[0].VolumeMounts[0].ReadOnly {
				t.Fatal("init regained shared/concurrent write access")
			}
		})
	}
}

func TestInitWorkspaceAndSourceProtectionWithLocalTools(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := &w.Spec.Templates[0]
		leaf.Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: "readonly"}
		leaf.Volumes = []corev1.Volume{{Name: "local-tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("32Mi"))}}}}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "local-tools", MountPath: "/tools", ReadOnly: true}}
		leaf.InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "populate", Image: leaf.Container.Image, Command: []string{"/bin/true"}, VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: "/workspace-alias"}, {Name: "local-tools", MountPath: "/tools"}}}}}
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	var source, workspace, tools bool
	for _, m := range wf.Spec.Templates[0].InitContainers[0].VolumeMounts {
		switch {
		case m.Name == SourceVolumeName:
			source = true
			if !m.ReadOnly {
				t.Error("source writable")
			}
		case sharedWorkspaceMount(m):
			workspace = true
			if !m.ReadOnly {
				t.Errorf("workspace writable: %+v", m)
			}
		case m.Name == "local-tools":
			tools = true
			if m.ReadOnly {
				t.Error("local init tools readonly")
			}
		}
	}
	if !source || !workspace || !tools {
		t.Fatalf("missing mounts: source=%v workspace=%v tools=%v", source, workspace, tools)
	}
}
