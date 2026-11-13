package argojob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Replay the authenticated upstream pre-patch Pod with the newly compiled
// patch, including hostile executor security defaults from the real controller.
func TestNonrootNewHelpersHaveBudgets(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		t.Run(shape, func(t *testing.T) {
			wf := nonrootBuiltFixture(t, shape == "script")
			pod := nonrootApplyControllerFixture(t, nonrootControllerFixture(t, shape+"-before-patch.json"), wf)
			for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
				switch c.Name {
				case executorTokenWaiter:
					assertControlContainer(t, c, false)
				}
			}
			// A named nonroot leaf stamps the entire Workflow; ordinary siblings must
			// keep the token waiter budget without gaining the leaf's nonroot identity.
			var ordinary corev1.PodSpec
			if err := json.Unmarshal([]byte(wf.Spec.Templates[1].PodSpecPatch), &ordinary); err != nil {
				t.Fatal(err)
			}
			for _, c := range ordinary.InitContainers {
				if c.Name == executorTokenWaiter {
					assertControlContainer(t, c, false)
				}
			}
			for _, template := range wf.Spec.Templates {
				assertNativeExecutorPatch(t, template.PodSpecPatch)
			}
			if ordinary.SecurityContext != nil {
				t.Fatal("ordinary sibling gained a nonroot Pod context")
			}
			for _, c := range pod.Spec.Containers {
				if c.Name == "wait" && !reflect.DeepEqual(c.SecurityContext, nonrootContainerSecurity(nonrootUID)) {
					t.Fatal("helper budget changed nonroot security")
				}
			}
		})
	}
}

// Re-enter Create through a fresh controller and the persisted API object.
func TestNonrootHelperPolicyRecovery(t *testing.T) {
	for _, policy := range []string{helperStoragePolicy} {
		t.Run(policy, func(t *testing.T) {
			kube := fake.NewClientset()
			runningSeedPods(kube)
			config := testConfig()

			workflows := &persistedWorkflowClient{}
			var seeds []seedExecCall
			newController := func() *Controller {
				c, err := NewController(workflows, kube, config)
				if err != nil {
					t.Fatal(err)
				}
				c.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
				return c
			}
			controller := newController()
			request := testRequest(pipeline.TriggerCI, nonrootTestDocument)
			request.SourceDir = writeCheckout(t)
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			before := workflows.existing.DeepCopy()
			if before.Annotations[helperStorageAnnotation] != policy {
				t.Fatal("fixture policy mismatch")
			}
			count := len(seeds)

			controller = newController()
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal("restart recovery failed", err)
			}
			if !reflect.DeepEqual(before, workflows.existing) || len(seeds) != count || workflows.creates != 1 {
				t.Fatal("recovery mutated workflow/source")
			}
			// Exact spec and policy checks remain.
			for _, mutation := range []string{"budget", "policy"} {
				t.Run(mutation, func(t *testing.T) {
					workflows.existing = before.DeepCopy()
					switch mutation {
					case "budget":
						workflows.existing.Spec.Templates[0].PodSpecPatch = strings.ReplaceAll(workflows.existing.Spec.Templates[0].PodSpecPatch, `"resources":{`, `"resources":{"claims":[{"name":"injected"}],`)
					case "policy":
						workflows.existing.Annotations[helperStorageAnnotation] = "unknown-policy"
					}
					if _, err := controller.Create(t.Context(), request); err == nil {
						t.Fatal("changed authority/spec accepted")
					}
					if len(seeds) != count || workflows.creates != 1 {
						t.Fatal("refusal mutated source/workflow")
					}
				})
			}
		})
	}
}

func TestNonrootNewBuildCannotSelectHistoricalBudgets(t *testing.T) {
	for _, authored := range []string{"", "argo-256Mi-512Mi-v1", "argo-256Mi-512Mi-v2", "argo-256Mi-512Mi-v3", helperStoragePolicy, "unknown-policy"} {
		source := strings.Replace(nonrootTestDocument, "    oberth.ci/nonroot-templates: test", "    oberth.ci/nonroot-templates: test\n    oberth.ci/helper-storage: "+authored, 1)
		request := testRequest(pipeline.TriggerCI, source)
		config := testConfig()

		if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "metadata.annotations") {
			t.Fatalf("authored policy admitted: %v", err)
		}

	}
}
