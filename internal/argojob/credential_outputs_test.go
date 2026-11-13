package argojob

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func credentialOutputRequest(t *testing.T, cloud, script bool, output string) (Config, Request) {
	t.Helper()
	mutate := func(wf *wfv1.Workflow) {
		leaf := &wf.Spec.Templates[0]
		if len(wf.Spec.VolumeClaimTemplates) == 0 {
			wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "work"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}}
		}
		if !cloud {
			leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
			leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
		}
		leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/shared/./data/", SubPath: "shared"}}
		if script {
			leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
			leaf.Container = nil
		}
		leaf.Outputs.Parameters = []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: output}}}
	}
	if cloud {
		request := wifTestRequest(t, mutate)
		request.SourceVolume = SourceVolume{ClaimName: "source-claim", SubPath: "source"}
		return wifTestConfig(), request
	}
	return testConfig(), isolationRequest(t, mutate)
}

func TestCredentialOutputPathsRefuseMountedReads(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, script := range []bool{false, true} {
			name := "secret/container"
			if cloud {
				name = "cloud/container"
			}
			if script {
				name = strings.Replace(name, "container", "script", 1)
			}
			for _, output := range []string{"/shared/data", "/shared/data/link", "/shared/unused/../data/link", "shared/data/link", "/shared/data//link", "/work/src/link", "{{inputs.parameters.path}}", SecretsMountPath + "/secret", ReleaseTokenMountPath + "/token", releaseWIFPath + "/token"} {
				// Each credential type has its own generated mounts.
				if cloud && (strings.HasPrefix(output, SecretsMountPath) || strings.HasPrefix(output, ReleaseTokenMountPath)) {
					continue
				}
				if !cloud && strings.HasPrefix(output, releaseWIFPath) {
					continue
				}
				t.Run(name+output, func(t *testing.T) {
					config, request := credentialOutputRequest(t, cloud, script, output)
					_, err := Build(config, request)
					if err == nil || !strings.Contains(err.Error(), "output read path") {
						t.Fatalf("mounted output was admitted: %v", err)
					}
				})
			}
		}
	}
}

func TestCredentialOutputSymlinkFromUngrantedStepRefused(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, relative := range []bool{false, true} {
			name := "secret/absolute"
			if cloud {
				name = "cloud/absolute"
			}
			if relative {
				name = strings.Replace(name, "absolute", "relative", 1)
			}
			t.Run(name, func(t *testing.T) {
				// Model wait's mount namespace: an earlier ungranted writer on the
				// shared claim links to a predictable credential mirror in mainctrfs.
				root := t.TempDir()
				credential := filepath.Join(root, "mainctrfs", "run", "credential", "token")
				shared := filepath.Join(root, "mainctrfs", "shared", "data")
				if err := os.MkdirAll(filepath.Dir(credential), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(shared, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(credential, []byte("credential-sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				target := credential
				if relative {
					var err error
					target, err = filepath.Rel(shared, credential)
					if err != nil {
						t.Fatal(err)
					}
				}
				link := filepath.Join(shared, "result")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(link)
				if err != nil || string(data) != "credential-sentinel" {
					t.Fatalf("symlink shape did not expose mirror: %q, %v", data, err)
				}
				config, request := credentialOutputRequest(t, cloud, false, "/shared/data/result")
				_, err = Build(config, request)
				if err == nil || !strings.Contains(err.Error(), "output read path") {
					t.Fatalf("shared symlink output was admitted: %v", err)
				}
			})
		}
	}
}

func TestCredentialRootFilesystemOutputsPass(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, output := range []string{"/root-result", "/shared/database/result"} {
			config, request := credentialOutputRequest(t, cloud, false, output)
			if _, err := Build(config, request); err != nil {
				t.Fatalf("plain output refused: %v", err)
			}
		}
	}
	// Ungranted leaves may still use native mounted output parameters.
	request := isolationRequest(t, func(wf *wfv1.Workflow) {
		wf.Spec.Templates[0].Outputs.Parameters = []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: "/work/src/result"}}}
	})
	if _, err := Build(testConfig(), request); err != nil {
		t.Fatal(err)
	}
}

func TestAuthoredFSGroupPreservesReadOnlySourceClaim(t *testing.T) {
	request := isolationRequest(t, func(wf *wfv1.Workflow) {
		wf.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To(int64(65534)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)}
	})
	wf, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := &corev1.PodSecurityContext{FSGroup: ptr.To(int64(65534)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)}
	if !reflect.DeepEqual(wf.Spec.SecurityContext, want) {
		t.Fatalf("authored fsGroup changed: %+v", wf.Spec.SecurityContext)
	}
	main := wf.Spec.Templates[0].Container
	pod := applyCredentialWaitPatch(t, credentialWaitControllerShape(*main), wf.Spec.Templates[0].PodSpecPatch)
	for _, c := range pod.Containers {
		found := false
		for _, m := range c.VolumeMounts {
			if m.Name == SourceVolumeName && (m.MountPath == SourceMountPath || m.MountPath == filepath.Join(argoMainFilesystem, SourceMountPath)) {
				found = true
				if !m.ReadOnly {
					t.Errorf("fsGroup source mount became writable in %s", c.Name)
				}
			}
		}
		if !found {
			t.Fatalf("source mount missing in %s", c.Name)
		}
	}
}
