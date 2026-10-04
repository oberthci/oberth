package argojob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// The actual v4.0.8 controller operated on these exact Build outputs using
// fake API clients. This test pins current inputs to that observation; it does
// not simulate controller construction or claim fresh cluster admission.
func TestIsolationMatchesActualControllerObservation(t *testing.T) {
	const directory = "testdata/isolation-argo4.0.8"
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var provenance struct {
		Path, Version, Sum, GoModSum string
		Files                        map[string]string `json:"files"`
	}
	if err := json.Unmarshal(read("provenance.json"), &provenance); err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile("../../go.sum")
	if err != nil {
		t.Fatal(err)
	}
	module := provenance.Path + " " + provenance.Version
	if !strings.Contains(string(mod), "\t"+module+"\n") {
		t.Fatal("Argo version changed; regenerate actual controller proof")
	}
	for _, line := range []string{module + " " + provenance.Sum, module + "/go.mod " + provenance.GoModSum} {
		if !strings.Contains("\n"+string(sums), "\n"+line+"\n") {
			t.Fatal("Argo module checksum changed; regenerate controller proof")
		}
	}
	fixture := func(name string, into any) {
		t.Helper()
		data := read(name)
		digest := sha256.Sum256(data)
		if provenance.Files[name] != hex.EncodeToString(digest[:]) {
			t.Fatalf("fixture integrity mismatch: %s", name)
		}
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatal(err)
		}
	}
	for _, shape := range []string{"dedicated-tools", "none", "container", "script", "containerset", "sidecars", "inline-dag", "inline-steps"} {
		t.Run(shape, func(t *testing.T) {
			var input wfv1.Workflow
			fixture(shape+"-workflow.json", &input)
			current, err := Build(testConfig(), isolationControllerRequest(t, shape))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current, &input) {
				t.Fatal("Build output changed; rerun actual controller capture before updating proof")
			}
			var before, submitted corev1.Pod
			fixture(shape+"-before-patch.json", &before)
			fixture(shape+"-submitted.json", &submitted)
			if submitted.Spec.AutomountServiceAccountToken == nil || *submitted.Spec.AutomountServiceAccountToken {
				t.Fatal("pipeline token automount was not disabled")
			}
			tokens := map[string]bool{}
			for _, v := range submitted.Spec.Volumes {
				if v.Secret != nil || v.Projected != nil || v.Name == executorTokenVolume {
					tokens[v.Name] = true
				}
			}
			mirrors := 0
			for _, c := range append(submitted.Spec.Containers, submitted.Spec.InitContainers...) {
				executor := c.Name == "init" || c.Name == "wait" || c.Name == executorTokenWaiter
				paths := map[string]bool{}
				for _, m := range c.VolumeMounts {
					if paths[m.MountPath] {
						t.Errorf("%s has duplicate mountPath %s: Kubernetes rejects this Pod", c.Name, m.MountPath)
					}
					paths[m.MountPath] = true
					if m.Name == "work" || m.Name == "release-tools" {
						if shape == "none" || !m.ReadOnly {
							t.Errorf("%s unexpected writable/shared mount: %+v", c.Name, m)
						}
						if c.Name == "wait" {
							mirrors++
						}
					}
					if !executor && (tokens[m.Name] || m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName || m.MountPath == VaultCAMountPath || m.MountPath == OberthBinMountPath) {
						t.Errorf("%s received credential mount %+v", c.Name, m)
					}
				}
				if !executor {
					env := map[string]string{}
					for _, e := range c.Env {
						env[e.Name] = e.Value
						if strings.HasPrefix(e.Name, "VAULT_") || e.Name == "OBERTH_VAULT_ROLE" {
							t.Errorf("%s received %s", c.Name, e.Name)
						}
					}
					if env["PATH"] != "/private/bin" || env["GOCACHE"] != "/private/cache" || env["OBERTH_SHA"] != testSHA {
						t.Errorf("%s lost private environment or run identity", c.Name)
					}
				}
			}
			if shape == "none" {
				return
			}
			if mirrors == 0 {
				t.Fatal("observation did not cover wait workspace mirrors")
			}
			// Negative control: upstream really made protected mirrors writable.
			unsafe := false
			for _, c := range before.Spec.Containers {
				if c.Name == "wait" {
					for _, m := range c.VolumeMounts {
						if (m.Name == "work" || m.Name == "release-tools") && !m.ReadOnly {
							unsafe = true
						}
					}
				}
			}
			if !unsafe {
				t.Fatal("capture did not exercise the controller's writable mirror mutation")
			}
			var leaf *wfv1.Template
			walkTemplates(current, func(tmpl *wfv1.Template) {
				if tmpl.Container != nil || tmpl.Script != nil || tmpl.ContainerSet != nil {
					leaf = tmpl
				}
			})
			if leaf == nil || leaf.PodSpecPatch == "" {
				t.Fatal("current Build has no executor protection")
			}
			raw, err := json.Marshal(before.Spec)
			if err != nil {
				t.Fatal(err)
			}
			patched, err := strategicpatch.StrategicMergePatch(raw, []byte(leaf.PodSpecPatch), corev1.PodSpec{})
			if err != nil {
				t.Fatal(err)
			}
			var spec corev1.PodSpec
			if err = json.Unmarshal(patched, &spec); err != nil {
				t.Fatal(err)
			}
			for _, c := range spec.Containers {
				if c.Name == "wait" {
					for _, m := range c.VolumeMounts {
						if (m.Name == "work" || m.Name == "release-tools") && !m.ReadOnly {
							t.Error("current patch left an observed mirror writable")
						}
					}
				}
			}
		})
	}
}
