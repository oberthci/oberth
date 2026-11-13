package argojob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// A missing main limit does not make the Pod unlimited once wait is bounded:
// kubelet compares total local usage (including disk emptyDirs) with the sum
// of container limits. Explicitly inspect admitted Pod resources, not usage
// report defaults that can label an unbounded main with the helper's request.
func TestOberthEveryLeafHasMainStorageBudget(t *testing.T) {
	for _, file := range []string{"build.yaml", "release.yaml"} {
		t.Run(file, func(t *testing.T) {
			trigger, ref := pipeline.TriggerCI, "refs/heads/main"
			if file == "release.yaml" {
				trigger, ref = pipeline.TriggerRelease, "refs/tags/v0.1.0"
			}
			wf, err := Build(oberthConfig(), Request{RunID: "storage-proof", Name: "storage-proof", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: ref, SHA: testSHA, Trigger: trigger, Source: loadPipeline(t, file), ApprovedSecrets: oberthReleaseApprovedSecrets()})
			if err != nil {
				t.Fatal(err)
			}
			if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
				t.Fatal("not current helper policy")
			}
			if directory := os.Getenv("OBERTH_STORAGE_FIXTURE_DIR"); directory != "" {
				body, err := json.MarshalIndent(wf, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, file+".json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			count := 0
			walkTemplates(wf, func(leaf *wfv1.Template) {
				main := leaf.Container
				if leaf.Script != nil {
					main = &leaf.Script.Container
				}
				if main == nil {
					return
				}
				count++
				t.Run(leaf.Name, func(t *testing.T) {
					request, limit := main.Resources.Requests.StorageEphemeral(), main.Resources.Limits.StorageEphemeral()
					minimum := resource.MustParse("512Mi")
					if request.Sign() <= 0 || limit.Cmp(minimum) < 0 || request.Cmp(*limit) > 0 {
						t.Fatalf("explicit ordered main storage with a 512Mi minimum limit required: %s/%s", request, limit)
					}
					before := credentialWaitControllerShape(*main.DeepCopy())
					before.Containers[1].Name = "main"
					after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
					var total, wait int64
					for _, c := range after.Containers {
						total += c.Resources.Limits.StorageEphemeral().Value()
						if c.Name == "wait" {
							wait = c.Resources.Limits.StorageEphemeral().Value()
							assertBoundedControlContainer(t, c)
						}
					}
					if wait != 512<<20 || total < limit.Value()+wait {
						t.Fatal("admitted Pod lost its main or helper storage allowance")
					}
					// Preserve the real release92 counterfactual: unbounded main leaves
					// only wait's 512Mi aggregate, below a valid main scratch allocation.
					if total-limit.Value() != wait {
						t.Fatal("unexpected steady-state sidecar storage requires review")
					}
				})
			})
			if count < 30 {
				t.Fatal("workflow leaf census incomplete")
			}
		})
	}
}

func TestOberthHelmPodStorageCoversObservedWorkingSet(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v1"}[legacy], func(t *testing.T) {
			wf, err := Build(oberthConfig(), Request{RunID: "helm-storage", Name: "helm-storage", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: "refs/tags/v0.1.0", SHA: testSHA, Trigger: pipeline.TriggerRelease, Source: loadPipeline(t, "release.yaml"), ApprovedSecrets: oberthReleaseApprovedSecrets()})
			if err != nil {
				t.Fatal(err)
			}
			for _, leaf := range wf.Spec.Templates {
				if leaf.Name != "install-helm" {
					continue
				}
				main := *leaf.Container.DeepCopy()
				main.Name = "main"
				pod := applyCredentialWaitPatch(t, credentialWaitControllerShape(main), leaf.PodSpecPatch)
				const observedLowerBound int64 = 1987485696
				var total int64
				for _, c := range pod.Containers {
					total += c.Resources.Limits.StorageEphemeral().Value()
				}
				if total <= observedLowerBound {
					t.Fatal("admitted aggregate remains below the observed release91 lower bound")
				}
				// Retain full existing private compiler/cache scratch capacity until a
				// representative cold volume cohort can justify tighter main budgets.
				capacity := resource.MustParse("8Gi")
				if main.Resources.Limits.StorageEphemeral().Cmp(capacity) < 0 {
					t.Fatal("cold Go bootstrap cannot retain its private scratch capacity")
				}
				if total-main.Resources.Limits.StorageEphemeral().Value() >= observedLowerBound {
					t.Fatal("counterfactual does not reproduce helper-only eviction")
				}
				return
			}
			t.Fatal("install-helm missing")
		})
	}
}
