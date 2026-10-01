package argojob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// Captured from the pinned upstream controller, not a reimplementation. Fake
// API clients prove construction only; cloud exchange is a separate gate.
func TestReleaseWIFMatchesActualControllerObservation(t *testing.T) {
	const directory = "testdata/wif-argo4.0.8"
	read := func(t *testing.T, name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var provenance struct {
		Path, Version, Sum, GoModSum string
		Files                        map[string]string
	}
	if err := json.Unmarshal(read(t, "provenance.json"), &provenance); err != nil {
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
		t.Fatal("controller version changed: regenerate proof")
	}
	for _, line := range []string{module + " " + provenance.Sum, module + "/go.mod " + provenance.GoModSum} {
		if !strings.Contains("\n"+string(sums), "\n"+line+"\n") {
			t.Fatal("controller checksum changed")
		}
	}
	for name, expected := range provenance.Files {
		digest := sha256.Sum256(read(t, name))
		if hex.EncodeToString(digest[:]) != expected {
			t.Fatalf("proof integrity mismatch: %s", name)
		}
	}
	decode := func(t *testing.T, name string, into any) {
		t.Helper()
		if provenance.Files[name] == "" {
			t.Fatalf("unbound fixture %s", name)
		}
		if err := json.Unmarshal(read(t, name), into); err != nil {
			t.Fatal(err)
		}
	}
	for _, shape := range wifControllerShapes {
		t.Run(shape, func(t *testing.T) {
			c, r := wifControllerInput(t, shape)
			current, err := Build(c, r)
			if err != nil {
				t.Fatal(err)
			}
			var input wfv1.Workflow
			decode(t, shape+"-workflow.json", &input)
			if !reflect.DeepEqual(current, &input) {
				t.Fatal("Build changed: recapture actual controller proof")
			}
			for _, automount := range []bool{false, true} {
				name := shape + "-sa-" + strconv.FormatBool(automount)
				var before, submitted corev1.Pod
				decode(t, name+"-before-patch.json", &before)
				decode(t, name+"-submitted.json", &submitted)
				assertWIFPodIsolation(t, shape, submitted.Spec)
				if shape == "sibling" {
					continue
				}
				unsafe := false
				for _, container := range before.Spec.Containers {
					if container.Name == "wait" {
						for _, mount := range container.VolumeMounts {
							unsafe = unsafe || mount.Name == releaseWIFVolume
						}
					}
				}
				if !unsafe {
					t.Fatal("negative control did not observe upstream wait credential mirror")
				}
				raw, err := json.Marshal(before.Spec)
				if err != nil {
					t.Fatal(err)
				}
				patched, err := strategicpatch.StrategicMergePatch(raw, []byte(current.Spec.Templates[0].PodSpecPatch), corev1.PodSpec{})
				if err != nil {
					t.Fatal(err)
				}
				var repaired corev1.PodSpec
				if err := json.Unmarshal(patched, &repaired); err != nil {
					t.Fatal(err)
				}
				assertWIFPodIsolation(t, shape, repaired)
			}
		})
	}
}

func assertWIFPodIsolation(t *testing.T, shape string, spec corev1.PodSpec) {
	t.Helper()
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("ambient API token enabled")
	}
	main, executor := false, false
	for _, c := range append(spec.Containers, spec.InitContainers...) {
		found := map[string]bool{}
		paths := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if paths[m.MountPath] {
				t.Fatalf("duplicate mount path %s", m.MountPath)
			}
			paths[m.MountPath] = true
			found[m.Name] = true
			if m.Name == releaseWIFVolume && (!m.ReadOnly || c.Name != "main" || shape == "sibling") {
				t.Fatal("WIF credential exposed outside approved main")
			}
			if c.Name != "main" && (m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName) {
				t.Fatal("secretstore credential leaked to helper/executor")
			}
			if m.Name == "exec-sa-token" {
				if c.Name != "init" && c.Name != "wait" {
					t.Fatal("executor token leaked")
				}
				executor = true
			}
		}
		if c.Name == "main" {
			main = true
			if found[releaseWIFVolume] != (shape != "sibling") {
				t.Fatal("main capability mismatch")
			}
			if shape == "vault" && (!found[SecretsVolumeName] || !found[ReleaseTokenVolumeName]) {
				t.Fatal("approved secretstore path lost")
			}
		}
	}
	if !main || !executor {
		t.Fatal("observation lost main or independent executor")
	}
}
