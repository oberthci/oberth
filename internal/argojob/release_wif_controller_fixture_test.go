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
			var input wfv1.Workflow
			decode(t, shape+"-workflow.json", &input)
			// The captured bytes carry their own identity; only then may the
			// removed annotation be renamed and the identity follow it.
			if len(input.Annotations[identityAnnotation]) != 64 {
				t.Fatal("captured workflow lost its server identity")
			}
			if shape != "vault" {
				if err := validateHelperStorage(&input); err == nil {
					t.Fatal("historical helper policy accepted for recovery")
				}
			} else if err := validateHelperStorage(&input); err != nil {
				t.Fatal("current vault capture lost its helper policy", err)
			}
			current, err := Build(c, r)
			if err != nil {
				t.Fatal(err)
			}
			if shape == "vault" {
				// This authenticated capture predates initializer runtime
				// protection. Model only that explicit new server patch;
				// preserve every other captured field and isolation assertion.
				capturedComparison := wifCaptureWithReadOnlyInitializers(t, &input)
				currentComparison := current.DeepCopy()
				historicalPodNameInput(t, currentComparison)
				withoutCapturedWorkflowRunFacts(t, capturedComparison)
				withoutCapturedWorkflowRunFacts(t, currentComparison)
				capturedJSON, _ := json.Marshal(capturedComparison)
				currentJSON, _ := json.Marshal(currentComparison)
				if string(capturedJSON) != string(currentJSON) {
					evidence := t.ArtifactDir()
					if err := os.WriteFile(filepath.Join(evidence, "expected-input.json"), capturedJSON, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(evidence, "current-input.json"), currentJSON, 0o600); err != nil {
						t.Fatal(err)
					}
					t.Fatal("vault capture input differs from current Build")
				}
				for _, name := range []string{"vault-workflow.json", "vault-sa-false-before-patch.json", "vault-sa-false-submitted.json", "vault-sa-true-before-patch.json", "vault-sa-true-submitted.json"} {
					if strings.Contains(string(read(t, name)), "oberth-store-") {
						t.Fatalf("%s retains a removed settings-map reference", name)
					}
				}
			}
			for _, automount := range []bool{false, true} {
				name := shape + "-sa-" + strconv.FormatBool(automount)
				var before, submitted corev1.Pod
				decode(t, name+"-before-patch.json", &before)
				decode(t, name+"-submitted.json", &submitted)
				if shape == "vault" {
					if submitted.Spec.PriorityClassName != "" || submitted.Spec.Priority != nil || submitted.Spec.PreemptionPolicy != nil {
						t.Fatal("current vault capture acquired unauthored scheduling fields")
					}
				} else {
					assertHistoricalWorkflowPriority(t, &submitted)
				}
				assertWIFPodIsolation(t, shape, submitted.Spec)
				if shape == "wif-only-step" && submitted.Spec.ServiceAccountName != "release-step-main" {
					t.Fatal("Argo lost pinned step identity")
				}
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
	var mainArgs []string
	for _, c := range spec.Containers {
		if c.Name == "main" {
			mainArgs = c.Args
		}
	}
	main, executor := false, false
	for _, c := range append(spec.Containers, spec.InitContainers...) {
		if c.Name != "main" {
			assertNoStoreSettings(t, c)
			if shape == "vault" {
				for _, e := range c.Env {
					if e.Name == "ARGO_TEMPLATE" {
						var view wfv1.Template
						if err := json.Unmarshal([]byte(e.Value), &view); err != nil {
							t.Fatal(err)
						}
						if view.Container == nil || !reflect.DeepEqual(view.Container.Args, mainArgs) {
							t.Fatalf("executor metadata changed native parameter substitution: %v vs %v", view.Container, mainArgs)
						}
					}
				}
			}
		}
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
				if c.Name != "init" && c.Name != "wait" && c.Name != executorTokenWaiter {
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

// renameRemovedSizeAnnotation moves a captured Workflow's removed
// oberth.ci/size annotation to the inert key the current fixture documents
// carry instead. Controller behaviour never read it, so the captured proof
// stays pinned; only this metadata key and the spec identity follow.
func renamedSpecIdentity(workflow *wfv1.Workflow) (string, error) {
	renameRemovedSizeAnnotation(workflow)
	return specIdentity(workflow)
}

func renameRemovedSizeAnnotation(workflow *wfv1.Workflow) {
	if size, ok := workflow.Annotations["oberth.ci/size"]; ok {
		delete(workflow.Annotations, "oberth.ci/size")
		workflow.Annotations["example.com/tier"] = size
	}
}

// Keep the historical bytes and native Pod observations immutable. The only
// expected input delta is the new authored-initializer runtime mount policy.
func wifCaptureWithReadOnlyInitializers(t *testing.T, captured *wfv1.Workflow) *wfv1.Workflow {
	t.Helper()
	expected := captured.DeepCopy()
	walkTemplates(expected, func(template *wfv1.Template) {
		if len(template.InitContainers) == 0 {
			return
		}
		var patch map[string]any
		if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
			t.Fatal(err)
		}
		initializers, _ := patch["initContainers"].([]any)
		order, _ := patch["$setElementOrder/initContainers"].([]any)
		for _, authored := range template.InitContainers {
			for _, entry := range initializers {
				if entry.(map[string]any)["name"] == authored.Name {
					t.Fatal("capture already contains authored initializer patch; remove historical transition")
				}
			}
			initializers = append(initializers, map[string]any{"name": authored.Name, "volumeMounts": []any{map[string]any{"name": "var-run-argo", "mountPath": "/var/run/argo", "readOnly": true}}})
			order = append(order, map[string]any{"name": authored.Name})
		}
		// Build serializes the captured waiter as a Container, so retain its
		// field order too: the spec identity includes the patch's JSON string.
		for i, entry := range initializers {
			if entry.(map[string]any)["name"] != executorTokenWaiter {
				continue
			}
			body, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			var waiter corev1.Container
			if err = json.Unmarshal(body, &waiter); err != nil {
				t.Fatal(err)
			}
			initializers[i] = waiter
		}
		patch["initContainers"], patch["$setElementOrder/initContainers"] = initializers, order
		body, err := json.Marshal(patch)
		if err != nil {
			t.Fatal(err)
		}
		template.PodSpecPatch = string(body)
	})
	identity, err := specIdentity(expected)
	if err != nil {
		t.Fatal(err)
	}
	expected.Annotations[identityAnnotation] = identity
	return expected
}
