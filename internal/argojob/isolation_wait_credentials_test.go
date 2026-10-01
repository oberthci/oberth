package argojob

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"
)

// Argo v4.0.8 copies every main mount into wait before applying PodSpecPatch,
// including on leaves with no outputs. This fixture models that exact order.
func credentialWaitControllerShape(main corev1.Container) corev1.PodSpec {
	waitMounts := []corev1.VolumeMount{{Name: "exec-sa-token", MountPath: ReleaseTokenMountPath, ReadOnly: true}}
	for _, mount := range main.VolumeMounts {
		mount.MountPath = path.Join(argoMainFilesystem, mount.MountPath)
		mount.ReadOnly = false
		waitMounts = append(waitMounts, mount)
	}
	return corev1.PodSpec{
		Containers:     []corev1.Container{{Name: argoWaitName, VolumeMounts: waitMounts}, main},
		InitContainers: []corev1.Container{{Name: argoInitName, VolumeMounts: []corev1.VolumeMount{{Name: "exec-sa-token", MountPath: ReleaseTokenMountPath, ReadOnly: true}}}},
	}
}

func applyCredentialWaitPatch(t *testing.T, before corev1.PodSpec, patch string) corev1.PodSpec {
	t.Helper()
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := strategicpatch.StrategicMergePatch(raw, []byte(patch), corev1.PodSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var after corev1.PodSpec
	if err := json.Unmarshal(merged, &after); err != nil {
		t.Fatal(err)
	}
	return after
}

func credentialMounts(container corev1.Container) []corev1.VolumeMount {
	var found []corev1.VolumeMount
	for _, mount := range container.VolumeMounts {
		if mount.Name == SecretsVolumeName || mount.Name == ReleaseTokenVolumeName {
			found = append(found, mount)
		}
	}
	return found
}

func TestWaitDropsOnlyCredentialMirrorsAfterArgoMutation(t *testing.T) {
	for _, hasOutput := range []bool{false, true} {
		name := "no-outputs"
		if hasOutput {
			name = "real-output-pvc"
		}
		t.Run(name, func(t *testing.T) {
			r := isolationRequest(t, func(w *wfv1.Workflow) {
				leaf := &w.Spec.Templates[0]
				leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
				leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
				if hasOutput {
					// Oberth admits real step output through the run's PVC,
					// while Argo output artifacts are denied at admission.
					leaf.Container.VolumeMounts = []corev1.VolumeMount{
						{Name: "work", MountPath: "/tmp/oberth-release", SubPath: "release"},
						{Name: "work", MountPath: "/private-input", SubPath: "input", ReadOnly: true},
					}
				}
				// Argo's authored mirror option is resolved before credentials are
				// injected, so these helpers must remain outside the chain.
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
			if leaf.PodSpecPatch == "" {
				t.Fatal("credentialed leaf has no server-only wait patch")
			}
			if got := len(credentialMounts(*leaf.Container)); got != 2 {
				t.Fatalf("main has %d credential mounts, want secret root and release token", got)
			}
			for _, helper := range []wfv1.UserContainer{leaf.Sidecars[0], leaf.InitContainers[0]} {
				if got := credentialMounts(helper.Container); len(got) != 0 {
					t.Errorf("authored helper received credentials: %+v", got)
				}
				if helper.MirrorVolumeMounts != nil && *helper.MirrorVolumeMounts {
					t.Error("authored helper can still mirror late credential mounts")
				}
			}
			before := credentialWaitControllerShape(*leaf.Container)
			if got := len(credentialMounts(before.Containers[0])); got != 2 {
				t.Fatalf("fixture did not model both unsafe Argo mirrors: %d", got)
			}
			after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
			wait := after.Containers[0]
			if got := credentialMounts(wait); len(got) != 0 {
				t.Errorf("wait retained a credential mirror: %+v", got)
			}
			if got := len(credentialMounts(after.Containers[1])); got != 2 {
				t.Errorf("patch removed main credentials: %d", got)
			}
			if got := credentialMounts(after.InitContainers[0]); len(got) != 0 {
				t.Errorf("Argo init received main credentials: %+v", got)
			}
			executorToken := false
			output := false
			input := false
			for _, mount := range wait.VolumeMounts {
				switch mount.MountPath {
				case ReleaseTokenMountPath:
					executorToken = mount.Name == "exec-sa-token" && mount.ReadOnly
				case "/mainctrfs/tmp/oberth-release":
					output = mount.Name == "work" && !mount.ReadOnly
				case "/mainctrfs/private-input":
					input = mount.Name == "work" && mount.ReadOnly
				}
			}
			if !executorToken || !output || (hasOutput && !input) {
				t.Errorf("patch lost executor token or ordinary output/input mounts: %+v", wait.VolumeMounts)
			}
		})
	}
}

func TestCredentialedArgoOutputArtifactRejectedBeforeWaitPatch(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := &w.Spec.Templates[0]
		leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
		leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
		leaf.Outputs.Artifacts = wfv1.Artifacts{{Name: "bundle", Path: "/run/oberth-secrets/bundle"}}
	})
	_, err := Build(testConfig(), r)
	if err == nil || !strings.Contains(err.Error(), "declares artifacts") {
		t.Fatalf("credentialed Argo output artifact was admitted: %v", err)
	}
}

// These are selected mount fields from two real Argo v4.0.8 admitted Pods;
// images, arguments, environment and volume contents are intentionally absent.
func TestWaitCredentialPatchMatchesAdmittedArgoPodShapes(t *testing.T) {
	for _, name := range []string{"cli142", "website105"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "wait-credential-argo4.0.8", name+"-podspec.json"))
			if err != nil {
				t.Fatal(err)
			}
			var before corev1.PodSpec
			if err := json.Unmarshal(data, &before); err != nil {
				t.Fatal(err)
			}
			var main, wait *corev1.Container
			for i := range before.Containers {
				switch before.Containers[i].Name {
				case "main":
					main = &before.Containers[i]
				case argoWaitName:
					wait = &before.Containers[i]
				}
			}
			if main == nil || wait == nil || len(credentialMounts(*main)) != 2 || len(credentialMounts(*wait)) != 2 {
				t.Fatal("admitted-Pod fixture does not cover both main and wait credential mounts")
			}
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: name, Container: main.DeepCopy()}}}}
			if err := protectExecutorWorkspaceMounts(wf); err != nil {
				t.Fatal(err)
			}
			if wf.Spec.Templates[0].PodSpecPatch == "" {
				t.Fatal("admitted credential shape has no server patch")
			}
			after := applyCredentialWaitPatch(t, before, wf.Spec.Templates[0].PodSpecPatch)
			var afterWait, afterMain *corev1.Container
			for i := range after.Containers {
				switch after.Containers[i].Name {
				case "main":
					afterMain = &after.Containers[i]
				case argoWaitName:
					afterWait = &after.Containers[i]
				}
			}
			if afterWait == nil || afterMain == nil || len(credentialMounts(*afterWait)) != 0 || len(credentialMounts(*afterMain)) != 2 {
				t.Fatalf("patch left wait credentials or removed main credentials: %+v", after.Containers)
			}
			for _, original := range wait.VolumeMounts {
				if original.Name == SecretsVolumeName || original.Name == ReleaseTokenVolumeName {
					continue
				}
				kept := false
				for _, current := range afterWait.VolumeMounts {
					if original.Name == current.Name && original.MountPath == current.MountPath && original.SubPath == current.SubPath {
						kept = true
						break
					}
				}
				if !kept {
					t.Errorf("patch removed unrelated executor/output mount %+v", original)
				}
			}
		})
	}
}

func TestWaitCredentialDeleteRequiresExactServerMountIdentity(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{
		Name: "ordinary", Container: &corev1.Container{VolumeMounts: []corev1.VolumeMount{
			{Name: "work", MountPath: SecretsMountPath},
			{Name: "work", MountPath: ReleaseTokenMountPath},
		}},
	}}}}
	if err := protectExecutorWorkspaceMounts(wf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wf.Spec.Templates[0].PodSpecPatch, `"$patch":"delete"`) {
		t.Fatal("ordinary mounts at similar paths were targeted as credentials")
	}
	if wf.Spec.Templates[0].PodSpecPatch != "" {
		t.Fatal("ordinary leaf acquired an unnecessary wait patch")
	}
}
