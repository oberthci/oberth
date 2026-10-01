package argojob

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func isolationRequest(t *testing.T, mutate func(*wfv1.Workflow)) Request {
	t.Helper()
	wf, err := argoworkflow.Decode([]byte(greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	wf.Annotations[argoworkflow.SecretPathsAnnotation] = "oberth/data/release/r2-upload-token"
	wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "work"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}}
	wf.Spec.Templates[0].Container.Env = append(wf.Spec.Templates[0].Container.Env, corev1.EnvVar{Name: "PATH", Value: "/private/bin"}, corev1.EnvVar{Name: "GOCACHE", Value: "/private/cache"})
	mutate(wf)
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	r := testRequest(periapsis.TriggerRelease, string(source))
	r.ApprovedSecrets = map[string]bool{"oberth/data/release/r2-upload-token": true}
	r.SourceVolume = SourceVolume{ClaimName: "source-claim", SubPath: "source", VaultCASubPath: "ca", BinarySubPath: "bin"}
	return r
}

func TestBuildPreservesTokenOptOut(t *testing.T) {
	for _, level := range []string{"template", "workflow", "defaults"} {
		t.Run(level, func(t *testing.T) {
			r := isolationRequest(t, func(wf *wfv1.Workflow) {
				switch level {
				case "template":
					wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(false)
				case "workflow":
					wf.Spec.AutomountServiceAccountToken = ptr.To(false)
					wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(true)
				case "defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{AutomountServiceAccountToken: ptr.To(false)}
					wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(true)
				}
			})
			wf, err := Build(testConfig(), r)
			if err != nil {
				t.Fatal(err)
			}
			tmpl := wf.Spec.Templates[0]
			if tmpl.AutomountServiceAccountToken == nil || *tmpl.AutomountServiceAccountToken {
				t.Fatal("test-only leaf regained service-account token")
			}
			if tmpl.ServiceAccountName != testCredentialedAcct {
				t.Fatal("forced identity changed")
			}
			for _, env := range tmpl.Container.Env {
				if strings.HasPrefix(env.Name, "VAULT_") || env.Name == "OBERTH_VAULT_ROLE" {
					t.Errorf("tokenless leaf received %s", env.Name)
				}
			}
			for _, m := range tmpl.Container.VolumeMounts {
				if m.Name == ReleaseTokenVolumeName || m.MountPath == VaultCAMountPath {
					t.Errorf("tokenless leaf received credential mount %+v", m)
				}
			}
		})
	}
}

func TestBuildWorkspaceIsolation(t *testing.T) {
	for _, mode := range []string{"none", "readonly"} {
		t.Run(mode, func(t *testing.T) {
			r := isolationRequest(t, func(wf *wfv1.Workflow) {
				tmpl := &wf.Spec.Templates[0]
				tmpl.Metadata.Annotations = map[string]string{"oberth.ci/workspace-mounts": mode, "oberth.ci/workspace-env": "none"}
				tmpl.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/alias", SubPath: "tools"}, {Name: "work", MountPath: "/parent"}}
			})
			config := testConfig()
			config.ReleaseCacheRoot = "/release-cache"
			wf, err := Build(config, r)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range wf.Spec.Templates[0].Container.VolumeMounts {
				if mode == "readonly" && (m.Name == "work" || m.Name == CacheVolumeName) && !m.ReadOnly {
					t.Errorf("writable shared mount %+v", m)
				}
				if mode == "none" && (m.Name == CacheVolumeName || m.MountPath == WorkspaceToolsMountPath || m.MountPath == WorkspaceReleaseMountPath) {
					t.Errorf("unrequested injected mount %+v", m)
				}
			}
			env := environmentOf(t, wf, "main")
			if env["PATH"] != "/private/bin" || env["GOCACHE"] != "/private/cache" || env["OBERTH_SHA"] != testSHA {
				t.Fatalf("isolation env = %v", env)
			}
		})
	}
}

func TestBuildPreservesReadonlyToolsAcrossAliases(t *testing.T) {
	r := isolationRequest(t, func(wf *wfv1.Workflow) {
		tmpl := &wf.Spec.Templates[0]
		tmpl.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: WorkspaceToolsMountPath, SubPath: "tools", ReadOnly: true}, {Name: "work", MountPath: "/alias", SubPath: "tools/bin"}, {Name: "work", MountPath: "/parent"}}
		tmpl.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "helper", Image: tmpl.Container.Image, Command: []string{"/bin/true"}, VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: "/other", SubPath: "tools"}}}}}
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := wf.Spec.Templates[0]
	for _, c := range []corev1.Container{*tmpl.Container, tmpl.Sidecars[0].Container} {
		for _, m := range c.VolumeMounts {
			if m.Name == "work" && (m.SubPath == "" || strings.HasPrefix(m.SubPath, "tools")) && !m.ReadOnly {
				t.Errorf("alias regained write access %+v", m)
			}
		}
	}
	if !strings.Contains(tmpl.PodSpecPatch, `"/mainctrfs/tmp/oberth-tools"`) {
		t.Fatal("wait mirror is not protected")
	}
}

func isolationControllerRequest(t *testing.T, shape string) Request {
	return isolationRequest(t, func(wf *wfv1.Workflow) {
		leaf := &wf.Spec.Templates[0]
		leaf.AutomountServiceAccountToken = ptr.To(false)
		leaf.Metadata.Annotations = map[string]string{"oberth.ci/workspace-mounts": "readonly", "oberth.ci/workspace-env": "none"}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/tools-alias", SubPath: "tools/bin"}, {Name: "work", MountPath: "/workspace-parent"}}
		switch shape {
		case "dedicated-tools":
			claim := wf.Spec.VolumeClaimTemplates[0].DeepCopy()
			claim.Name = "release-tools"
			wf.Spec.VolumeClaimTemplates = append(wf.Spec.VolumeClaimTemplates, *claim)
			leaf.Metadata.Annotations[argoworkflow.WorkspaceMountsAnnotation] = "none"
			leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "release-tools", MountPath: "/tools", SubPath: "bin", ReadOnly: true}, {Name: "release-tools", MountPath: "/tools-alias"}}
			helper := *leaf.Container.DeepCopy()
			helper.Name = "verify-tools"
			leaf.InitContainers = []wfv1.UserContainer{{Container: helper}}
		case "none":
			leaf.Metadata.Annotations[argoworkflow.WorkspaceMountsAnnotation] = "none"
			leaf.Container.VolumeMounts = nil
		case "script":
			leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
			leaf.Container = nil
		case "containerset":
			main := *leaf.Container
			main.Name = "main"
			other := *leaf.Container.DeepCopy()
			other.Name = "other"
			leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: main}, {Container: other}}}
			leaf.Container = nil
		case "sidecars":
			other := *leaf.Container.DeepCopy()
			other.Name = "helper"
			leaf.Sidecars = []wfv1.UserContainer{{Container: other, MirrorVolumeMounts: ptr.To(true)}}
			other.Name = "prepare"
			leaf.InitContainers = []wfv1.UserContainer{{Container: other, MirrorVolumeMounts: ptr.To(true)}}
		case "inline-dag", "inline-steps":
			inline := leaf.DeepCopy()
			inline.Name = "inline"
			leaf.Metadata.Annotations = nil
			leaf.Container = nil
			leaf.AutomountServiceAccountToken = nil
			if shape == "inline-dag" {
				leaf.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "test", Inline: inline}}}
			} else {
				leaf.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "test", Inline: inline}}}}
			}
		}
	})
}

func TestWorkspaceIsolationCoversEveryPodContainerShape(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerset", "sidecars", "inline-dag", "inline-steps"} {
		t.Run(shape, func(t *testing.T) {
			wf, err := Build(testConfig(), isolationControllerRequest(t, shape))
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			walkTemplates(wf, func(tmpl *wfv1.Template) {
				templateContainers(tmpl, func(c *corev1.Container) {
					count++
					if !tokenDisabled(tmpl) {
						t.Error("leaf regained token")
					}
					for _, m := range c.VolumeMounts {
						if sharedWorkspaceMount(m) && !m.ReadOnly {
							t.Errorf("%s writable %+v", c.Name, m)
						}
						if m.Name == ReleaseTokenVolumeName {
							t.Error("leaf regained projected token")
						}
					}
					for _, env := range c.Env {
						if env.Name == "PATH" && env.Value != "/private/bin" {
							t.Error("leaf lost private PATH")
						}
						if strings.HasPrefix(env.Name, "VAULT_") {
							t.Error("leaf received Vault env")
						}
					}
				})
			})
			if count == 0 {
				t.Fatal("fixture exercised no containers")
			}
		})
	}
}

func TestBuildRefusesContradictoryOrUnprovenIsolation(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(*wfv1.Workflow)
	}{
		{"unknown-mount-mode", "must be none or readonly", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Metadata.Annotations = map[string]string{"oberth.ci/workspace-mounts": "{{inputs.parameters.mode}}"}
		}},
		{"unknown-env-mode", "must be none", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Metadata.Annotations = map[string]string{"oberth.ci/workspace-env": "true"}
		}},
		{"other-annotation", "annotations is assigned", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Metadata.Annotations = map[string]string{"sidecar.istio.io/inject": "true"}
		}},
		{"defaults-mounts", "templateDefaults", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Sidecars: []wfv1.UserContainer{{Container: corev1.Container{Name: "helper", Image: w.Spec.Templates[0].Container.Image}}}}
			w.Spec.Templates[0].Metadata.Annotations = map[string]string{"oberth.ci/workspace-mounts": "none"}
		}},
		{"defaults-controls", "templateDefaults", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Annotations: map[string]string{"oberth.ci/workspace-mounts": "none"}}}
		}},
		{"credential-command", "disables its service-account token", func(w *wfv1.Workflow) {
			c := w.Spec.Templates[0].Container
			c.Command = []string{"oberth", "secretstore", "exec"}
			c.Args = []string{"--path", "oberth/data/release/r2-upload-token", "--", "/bin/true"}
			w.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Build(testConfig(), isolationRequest(t, test.mutate))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v; want %q", err, test.want)
			}
		})
	}
}

func TestWorkspaceNoneRetainsPrivateMountsAndSiblingDefaults(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		w.Spec.Templates = append(w.Spec.Templates, *w.Spec.Templates[0].DeepCopy())
		w.Spec.Templates[1].Name = "sibling"
		leaf := &w.Spec.Templates[0]
		leaf.Metadata.Annotations = map[string]string{"oberth.ci/workspace-mounts": "none"}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/private-test", SubPath: "test"}}
	})
	cfg := testConfig()
	cfg.ReleaseCacheRoot = "/release-cache"
	wf, err := Build(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if env := environmentOf(t, wf, "main"); env["PATH"] != "/private/bin" || env["GOCACHE"] != "/private/cache" {
		t.Fatal("none advertised absent cache or tools")
	}
	if env := environmentOf(t, wf, "sibling"); env["GOCACHE"] != BuildCachePath {
		t.Fatal("sibling lost its default environment")
	}
	var private bool
	for _, m := range wf.Spec.Templates[0].Container.VolumeMounts {
		if m.MountPath == "/private-test" {
			private = true
			if m.ReadOnly {
				t.Error("private test output unexpectedly readonly")
			}
		}
	}
	if !private {
		t.Fatal("none lost explicit private test output")
	}
}

func TestReadonlyWorkspaceCoversReplacementAndDynamicAliases(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{
			{Name: "work", MountPath: WorkspaceToolsMountPath, SubPath: "different-input", ReadOnly: true},
			{Name: "work", MountPath: "/alias", SubPath: "tools/bin"},
			{Name: "work", MountPath: "/dynamic", SubPathExpr: "$(DIRECTORY)"},
		}
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range wf.Spec.Templates[0].Container.VolumeMounts {
		if m.Name == "work" && (m.MountPath == WorkspaceToolsMountPath || m.MountPath == "/alias" || m.MountPath == "/dynamic") && !m.ReadOnly {
			t.Errorf("readonly request lost at %+v", m)
		}
	}
}

func TestIsolationRejectsLateCredentialDefaultsAndNonleafControls(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(*wfv1.Workflow)
	}{
		{"late-credential-defaults", "tokenless", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(false)
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Image: w.Spec.Templates[0].Container.Image, Command: []string{"oberth", "secretstore", "exec"}, Args: []string{"--path", "oberth/data/release/r2-upload-token", "--", "/bin/true"}}}
			w.Spec.Templates[0].Container.Command = nil
		}},
		{"parent-workspace-controls", "Pod leaf", func(w *wfv1.Workflow) {
			leaf := w.Spec.Templates[0].DeepCopy()
			leaf.Name = "leaf"
			w.Spec.Templates[0] = wfv1.Template{Name: "main", Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.WorkspaceMountsAnnotation: "none"}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "test", Inline: leaf}}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Build(testConfig(), isolationRequest(t, test.mutate))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v; want %s", err, test.want)
			}
		})
	}
}

func TestContainerSetCommonReadonlyMountProtectsNodeAliases(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := &w.Spec.Templates[0]
		main := *leaf.Container
		main.Name = "main"
		main.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/alias", SubPath: "tools/bin"}}
		leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: main}}, VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: WorkspaceToolsMountPath, SubPath: "tools", ReadOnly: true}}}
		leaf.Container = nil
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &wf.Spec.Templates[0]
	for _, m := range leaf.ContainerSet.GetContainers()[0].VolumeMounts {
		if m.Name == "work" && strings.HasPrefix(m.SubPath, "tools") && !m.ReadOnly {
			t.Errorf("common readonly mount lost protection: %+v", m)
		}
	}
	if leaf.PodSpecPatch == "" {
		t.Fatal("container set has no controller volume/mirror repair")
	}
}

func TestMirroringNeverCopiesCredentialChainMountsToHelpers(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := &w.Spec.Templates[0]
		leaf.Container.Command = []string{"oberth", "secretstore", "exec"}
		leaf.Container.Args = []string{"--path", "oberth/data/release/r2-upload-token", "--", "/bin/true"}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/private-input", SubPath: "input", ReadOnly: true}}
		helper := wfv1.UserContainer{Container: corev1.Container{Name: "helper", Image: leaf.Container.Image, Command: []string{"/bin/true"}}, MirrorVolumeMounts: ptr.To(true)}
		leaf.Sidecars = []wfv1.UserContainer{helper}
		helper.Name = "prepare"
		leaf.InitContainers = []wfv1.UserContainer{helper}
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &wf.Spec.Templates[0]
	hasToken := false
	for _, m := range leaf.Container.VolumeMounts {
		if m.Name == ReleaseTokenVolumeName {
			hasToken = true
		}
	}
	if !hasToken {
		t.Fatal("fixture did not exercise credential injection")
	}
	for _, c := range []wfv1.UserContainer{leaf.Sidecars[0], leaf.InitContainers[0]} {
		if c.MirrorVolumeMounts != nil && *c.MirrorVolumeMounts {
			t.Fatal("controller can still copy credential mounts")
		}
		input := false
		seen := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if seen[m.MountPath] {
				t.Errorf("duplicate helper mount %s", m.MountPath)
			}
			seen[m.MountPath] = true
			if m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName {
				t.Errorf("helper received %s", m.Name)
			}
			if m.MountPath == "/private-input" && m.ReadOnly {
				input = true
			}
		}
		if !input {
			t.Error("helper lost declared readonly mirror")
		}
	}
}

func TestArtifactReservedAliasCannotRestoreSourceWrite(t *testing.T) {
	for _, mode := range []string{"", "none", "readonly"} {
		wf, e := argoworkflow.Decode([]byte(greedyDocument))
		if e != nil {
			t.Fatal(e)
		}
		if mode != "" {
			wf.Spec.Templates[0].Metadata = wfv1.Metadata{Annotations: map[string]string{argoworkflow.WorkspaceMountsAnnotation: mode}}
		}
		wf.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: ArtifactsVolumeName, MountPath: "/source-write-alias", SubPath: "src"}}
		b, e := json.Marshal(wf)
		if e != nil {
			t.Fatal(e)
		}
		actual, e := Build(testConfig(), seededRequest(t, string(b)))
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, m := range actual.Spec.Templates[0].Container.VolumeMounts {
			if m.MountPath == "/source-write-alias" {
				t.Errorf("%s admits reserved source alias: %+v", mode, m)
			}
			if m.Name == ArtifactsVolumeName {
				found = true
				if m.SubPath != artifactsSubPath || m.MountPath != ArtifactsMountPath || m.ReadOnly != (mode == "readonly") {
					t.Errorf("wrong bounded artifact mount: %+v", m)
				}
			}
		}
		if found != (mode != "none") {
			t.Errorf("mode %q artifact mount presence %v", mode, found)
		}
		env := environmentOf(t, actual, "main")
		if (env["OBERTH_ARTIFACTS"] != "") != (mode != "none") {
			t.Errorf("mode %q artifact environment %v", mode, env)
		}
	}
}

func TestDedicatedToolsClaimReadonlyProtectsAliasesAndWait(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		claim := w.Spec.VolumeClaimTemplates[0].DeepCopy()
		claim.Name = "release-tools"
		w.Spec.VolumeClaimTemplates = append(w.Spec.VolumeClaimTemplates, *claim)
		leaf := &w.Spec.Templates[0]
		leaf.AutomountServiceAccountToken = ptr.To(false)
		leaf.Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: "none"}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "release-tools", MountPath: "/tools", SubPath: "bin", ReadOnly: true}, {Name: "release-tools", MountPath: "/tools-alias"}}
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &wf.Spec.Templates[0]
	for _, m := range leaf.Container.VolumeMounts {
		if m.Name == "release-tools" && !m.ReadOnly {
			t.Errorf("dedicated tools alias writable %+v", m)
		}
	}
	if !strings.Contains(leaf.PodSpecPatch, `"/mainctrfs/tools"`) {
		t.Fatal("dedicated tools wait mirror is unprotected")
	}
}

func TestLateDefaultMountsCannotBypassCredentialBoundary(t *testing.T) {
	for _, helperKind := range []string{"sidecar", "init"} {
		for _, inherited := range []bool{false, true} {
			for _, mirror := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/inherited=%v/mirror=%v", helperKind, inherited, mirror), func(t *testing.T) {
					r := isolationRequest(t, func(w *wfv1.Workflow) {
						leaf := &w.Spec.Templates[0]
						leaf.Container.Command = []string{"oberth", "secretstore", "exec"}
						leaf.Container.Args = []string{"--path", "oberth/data/release/r2-upload-token", "--", "/bin/true"}
						helper := wfv1.UserContainer{Container: corev1.Container{Name: "late-helper", Image: leaf.Container.Image, Command: []string{"/bin/true"}}}
						if inherited {
							if helperKind == "sidecar" {
								leaf.Sidecars = []wfv1.UserContainer{helper}
							} else {
								leaf.InitContainers = []wfv1.UserContainer{helper}
							}
						}
						if mirror {
							helper.MirrorVolumeMounts = ptr.To(true)
						} else {
							helper.VolumeMounts = []corev1.VolumeMount{{Name: ReleaseTokenVolumeName, MountPath: "/stolen-token"}}
						}
						w.Spec.TemplateDefaults = &wfv1.Template{}
						if helperKind == "sidecar" {
							w.Spec.TemplateDefaults.Sidecars = []wfv1.UserContainer{helper}
						} else {
							w.Spec.TemplateDefaults.InitContainers = []wfv1.UserContainer{helper}
						}
					})
					if _, err := Build(testConfig(), r); err == nil || !strings.Contains(err.Error(), "templateDefaults") {
						t.Fatalf("unsafe late defaults admitted: %v", err)
					}
				})
			}
		}
	}
}
