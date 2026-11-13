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

// Repository-authored main storage budgets remain independent of native
// executor defaults, which supply no storage request or limit.
func TestOberthEveryLeafHasMainStorageBudget(t *testing.T) {
	for _, file := range []string{"build.yaml", "release.yaml"} {
		t.Run(file, func(t *testing.T) {
			trigger, ref := pipeline.TriggerCI, "refs/heads/main"
			if file == "release.yaml" {
				trigger, ref = pipeline.TriggerRelease, "refs/tags/v0.1.0"
			}
			wf, err := buildOberthPipeline(oberthConfig(), Request{RunID: "storage-proof", Name: "storage-proof", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: ref, SHA: testSHA, Trigger: trigger, Source: loadPipeline(t, file), ApprovedSecrets: oberthReleaseApprovedSecrets()})
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
					before := nativeBudgetControllerShape(credentialWaitControllerShape(*main.DeepCopy()))
					before.Containers[1].Name = "main"
					after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
					var total, wait int64
					for _, c := range after.Containers {
						total += c.Resources.Limits.StorageEphemeral().Value()
						if c.Name == "wait" {
							wait = c.Resources.Limits.StorageEphemeral().Value()
							if len(c.Resources.Limits) != 0 || c.Resources.Requests.StorageEphemeral().Sign() != 0 {
								t.Fatal("native executor acquired a limit or storage request")
							}
						}
					}
					if wait != 0 || total != limit.Value() {
						t.Fatal("native defaults changed the authored steady-state storage budget", total, wait, limit.Value())
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
	for _, policy := range []string{helperStoragePolicy} {
		t.Run(policy, func(t *testing.T) {
			wf, err := buildOberthPipeline(oberthConfig(), Request{RunID: "helm-storage", Name: "helm-storage", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: "refs/tags/v0.1.0", SHA: testSHA, Trigger: pipeline.TriggerRelease, Source: loadPipeline(t, "release.yaml"), ApprovedSecrets: oberthReleaseApprovedSecrets()})
			if err != nil {
				t.Fatal(err)
			}
			for _, leaf := range wf.Spec.Templates {
				if leaf.Name != "install-helm" {
					continue
				}
				main := *leaf.Container.DeepCopy()
				main.Name = "main"
				pod := applyCredentialWaitPatch(t, nativeBudgetControllerShape(credentialWaitControllerShape(main)), leaf.PodSpecPatch)
				const observedLowerBound int64 = 1987485696
				var total int64
				for _, c := range pod.Containers {
					total += c.Resources.Limits.StorageEphemeral().Value()
				}
				if total <= observedLowerBound {
					t.Fatal("admitted aggregate remains below the observed release91 lower bound")
				}
				// Retain the repository's own compiler/cache scratch capacity.
				capacity := resource.MustParse("8Gi")
				if main.Resources.Limits.StorageEphemeral().Cmp(capacity) < 0 {
					t.Fatal("cold bootstrap lost its authored scratch capacity")
				}
				if total != main.Resources.Limits.StorageEphemeral().Value() {
					t.Fatal("native helper added storage limits to the Pod total")
				}
				return
			}
			t.Fatal("install-helm missing")
		})
	}
}
