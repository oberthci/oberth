package argojob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Replay the authenticated upstream pre-patch Pod with the newly compiled
// patch, including hostile executor security defaults from the real controller.
func TestPinnedProfileNewHelpersHaveBudgets(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		t.Run(shape, func(t *testing.T) {
			wf := nonrootBuiltFixture(t, shape == "script")
			pod := nonrootApplyControllerFixture(t, nonrootControllerFixture(t, shape+"-before-patch.json"), wf)
			for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
				switch c.Name {
				case "init", "wait", executorTokenWaiter:
					assertControlContainer(t, c, false)
				}
			}
			// A named nonroot leaf stamps the entire Workflow; ordinary siblings must
			// receive their budgets too, without gaining the leaf's nonroot identity.
			var ordinary corev1.PodSpec
			if err := json.Unmarshal([]byte(wf.Spec.Templates[1].PodSpecPatch), &ordinary); err != nil {
				t.Fatal(err)
			}
			for _, c := range append(ordinary.InitContainers, ordinary.Containers...) {
				assertControlContainer(t, c, false)
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

// Re-enter Create through a new controller, fresh controller verification and
// the persisted API object, not an in-memory request carrying legacy flags.
func TestPinnedProfileHelperPolicyRecovery(t *testing.T) {
	for _, policy := range []string{"", legacyHelperStoragePolicy, cappedHelperStoragePolicy, stepLimitHelperStoragePolicy, helperStoragePolicy} {
		t.Run(policy, func(t *testing.T) {
			pod, cm := nonrootVerifierObjects()
			kube := fake.NewClientset(pod, cm)
			runningSeedPods(kube)
			config := testConfig()
			config.NonrootProfile = argoworkflow.NonrootStaticProfile
			workflows := &nonrootWorkflowClient{}
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
			if err := recoverHelperStorage(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{helperStorageAnnotation: policy}}}, &request); err != nil {
				t.Fatal(err)
			}
			var err error
			request, err = controller.PrepareNonroot(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			before := workflows.existing.DeepCopy()
			if before.Annotations[helperStorageAnnotation] != policy {
				t.Fatal("fixture policy mismatch")
			}
			count := len(seeds)
			request.legacyHelperStorage, request.legacyProfileHelperResources, request.legacyHelperStorageLimits, request.legacyStepLimitHelperStorage = false, false, false, false
			request.nonrootProof = nil
			controller = newController()
			request, err = controller.PrepareNonroot(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal("restart recovery failed", err)
			}
			if !reflect.DeepEqual(before, workflows.existing) || len(seeds) != count || workflows.creates != 1 {
				t.Fatal("recovery mutated workflow/source")
			}
			// Exact spec, annotation and fresh controller authority refusals remain.
			for _, mutation := range []string{"budget", "policy", "proof", "controller"} {
				t.Run(mutation, func(t *testing.T) {
					workflows.existing = before.DeepCopy()
					switch mutation {
					case "budget":
						workflows.existing.Spec.Templates[0].PodSpecPatch = strings.ReplaceAll(workflows.existing.Spec.Templates[0].PodSpecPatch, `"resources":{`, `"resources":{"claims":[{"name":"injected"}],`)
					case "policy":
						workflows.existing.Annotations[helperStorageAnnotation] = "unknown-policy"
					case "proof":
						workflows.existing.Annotations[nonrootProfileAnnotation] = "forged"
					case "controller":
						changed := cm.DeepCopy()
						changed.UID = "replacement-config"
						if _, err := kube.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), changed, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
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

func TestPinnedProfileNewBuildCannotSelectHistoricalBudgets(t *testing.T) {
	for _, authored := range []string{"", legacyHelperStoragePolicy, cappedHelperStoragePolicy, stepLimitHelperStoragePolicy, helperStoragePolicy, "unknown-policy"} {
		source := strings.Replace(nonrootTestDocument, "    oberth.ci/nonroot-templates: test", "    oberth.ci/nonroot-templates: test\n    oberth.ci/helper-storage: "+authored, 1)
		request := nonrootFixtureProof(testRequest(pipeline.TriggerCI, source))
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		wf, err := Build(config, request)
		if err != nil {
			t.Fatal(err)
		}
		if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
			t.Fatal("authored policy selected recovery mode")
		}
		request.nonrootProof = nil
		if _, err := Build(config, request); err == nil {
			t.Fatal("new helper policy bypassed controller proof")
		}
	}
}

func TestPinnedHelperVersionChangesOnlyBudgets(t *testing.T) {
	for _, script := range []bool{false, true} {
		old := nonrootBuiltWithHelperPolicy(t, script, "", legacyHelperStoragePolicy)
		current := nonrootBuiltFixture(t, script)
		if old.Annotations[nonrootProfileAnnotation] != current.Annotations[nonrootProfileAnnotation] {
			t.Fatal("controller proof changed")
		}
		current.Annotations[helperStorageAnnotation] = legacyHelperStoragePolicy
		for i := range current.Spec.Templates {
			var patch map[string]any
			if err := json.Unmarshal([]byte(current.Spec.Templates[i].PodSpecPatch), &patch); err != nil {
				t.Fatal(err)
			}
			for _, group := range []string{"containers", "initContainers"} {
				items, _ := patch[group].([]any)
				retained := make([]any, 0, len(items))
				for _, item := range items {
					c := item.(map[string]any)
					switch c["name"] {
					case "init", "wait":
						delete(c, "resources")
					case executorTokenWaiter:
						c["resources"] = map[string]any{}
					}
					// Ordinary wait had no patch before v2.
					if c["name"] != "wait" || len(c) > 1 {
						retained = append(retained, c)
					}
				}
				if len(retained) > 0 {
					patch[group] = retained
				} else {
					delete(patch, group)
				}
			}
			encoded, err := json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			current.Spec.Templates[i].PodSpecPatch = string(encoded)
		}
		// Canonicalize both patch JSON representations before comparing fields;
		// the token waiter is originally encoded from a typed Container.
		for i := range old.Spec.Templates {
			var patch map[string]any
			if err := json.Unmarshal([]byte(old.Spec.Templates[i].PodSpecPatch), &patch); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			old.Spec.Templates[i].PodSpecPatch = string(encoded)
		}
		oldIdentity, err := specIdentity(old)
		if err != nil {
			t.Fatal(err)
		}
		old.Annotations[identityAnnotation] = oldIdentity
		identity, err := specIdentity(current)
		if err != nil {
			t.Fatal(err)
		}
		current.Annotations[identityAnnotation] = identity
		if !reflect.DeepEqual(old, current) {
			t.Fatal("helper policy changed fields beyond the three helper budgets")
		}
	}
}
