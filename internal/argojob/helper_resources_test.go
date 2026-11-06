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
)

func assertBoundedControlContainer(t *testing.T, container corev1.Container) {
	t.Helper()
	for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		request := container.Resources.Requests[key]
		limit := container.Resources.Limits[key]
		if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			t.Errorf("helper %s requires positive ordered %s requests and limits", container.Name, key)
		}
	}
	if container.Resources.Limits.Cpu().MilliValue() > 4000 || container.Resources.Limits.Memory().Value() > 8<<30 {
		t.Errorf("helper %s exceeds the container ceiling", container.Name)
	}
	if container.Resources.Limits.Cpu().MilliValue() < 500 || container.Resources.Limits.Memory().Value() < 512<<20 {
		t.Errorf("helper %s limits are below the 500m/512Mi execution floor", container.Name)
	}
}

func TestInjectedControlBudgetsCoverUnpinnedExecutorContainers(t *testing.T) {
	archiveLogs := true
	cases := []struct {
		name     string
		leaf     wfv1.Template
		wantWait bool
		wantMain bool
	}{
		{name: "container", leaf: wfv1.Template{Container: &corev1.Container{}}, wantWait: true},
		{name: "resource", leaf: wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create"}}, wantMain: true},
		{name: "resource-logs", leaf: wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create"}, ArchiveLocation: &wfv1.ArtifactLocation{ArchiveLogs: &archiveLogs}}, wantWait: true, wantMain: true},
		{name: "data", leaf: wfv1.Template{Data: &wfv1.Data{}}, wantMain: true},
	}
	for _, profile := range []string{"", "verified-fixture"} {
		for _, tc := range cases {
			t.Run(profile+"/"+tc.name, func(t *testing.T) {
				leaf := tc.leaf
				leaf.Name = "step"
				workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{nonrootProfileAnnotation: profile, helperStorageAnnotation: helperStoragePolicy}}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf}}}
				if err := projectExecutorTokens(workflow, testConfig(), true); err != nil {
					t.Fatal(err)
				}
				var patch corev1.PodSpec
				if err := json.Unmarshal([]byte(workflow.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
					t.Fatal(err)
				}
				seen := map[string]bool{}
				for _, container := range append(patch.InitContainers, patch.Containers...) {
					assertBoundedControlContainer(t, container)
					if seen[container.Name] {
						t.Errorf("duplicate helper %s", container.Name)
					}
					seen[container.Name] = true
				}
				for _, name := range []string{executorTokenWaiter, "init"} {
					if !seen[name] {
						t.Errorf("missing explicit %s budget", name)
					}
				}
				if seen["wait"] != tc.wantWait || seen["main"] != tc.wantMain {
					t.Errorf("executor shape has wait=%t main=%t, want wait=%t main=%t", seen["wait"], seen["main"], tc.wantWait, tc.wantMain)
				}
			})
		}
	}

}

// The live ordinary Argo v4.0.8 wait container had resources={}. Compile the
// default path (no ExecutionPlacement) and apply its patch after Argo mutation.
// A helper budget on init alone cannot fix the running Pod's aggregate limit.
func TestDefaultCompiledPodStorageIncludesSharedArgoHelper(t *testing.T) {
	config := testConfig()
	config.ExecutionPlacement = nil
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	source := resourceBoundsSource("      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi, ephemeral-storage: 128Mi}\n")
	wf, err := Build(config, resourceBoundsRequest(source))
	if err != nil {
		t.Fatal(err)
	}
	leaf := wf.Spec.Templates[0]
	main := *leaf.Container.DeepCopy()
	main.Name = "main"
	before := credentialWaitControllerShape(main)
	before.Volumes = append(before.Volumes, corev1.Volume{Name: "var-run-argo", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
	const measuredHelperBytes int64 = 160849920 // kubelet disk-volume receipt; not a whole-Pod peak
	var podRequest, podLimit, waitRequest, waitLimit int64
	for _, c := range after.Containers {
		req := c.Resources.Requests.StorageEphemeral()
		lim := c.Resources.Limits.StorageEphemeral()
		podRequest += req.Value()
		podLimit += lim.Value()
		switch c.Name {
		case "main":
			if !reflect.DeepEqual(c.Resources, main.Resources) {
				t.Fatal("workload resources changed")
			}
		case "wait":
			assertBoundedControlContainer(t, c)
			waitRequest, waitLimit = req.Value(), lim.Value()
		}
	}
	if waitRequest < measuredHelperBytes || waitLimit < measuredHelperBytes {
		t.Fatal("ordinary wait must reserve the measured shared helper bytes")
	}
	if podRequest != (64+256)<<20 || podLimit != (128+512)<<20 {
		t.Fatalf("unexpected running-Pod storage request/limit %d/%d", podRequest, podLimit)
	}
	// Counterfactual: init may still have a budget, but removing wait's storage
	// recreates the measured eviction condition before main uses any scratch.
	if podLimit-waitLimit >= measuredHelperBytes {
		t.Fatal("fixture does not reproduce missing-wait eviction")
	}
	for _, c := range after.InitContainers {
		assertBoundedControlContainer(t, c)
	}
	for _, v := range after.Volumes {
		if v.Name == "var-run-argo" && !reflect.DeepEqual(v, before.Volumes[0]) {
			t.Fatal("helper storage changed emptyDir policy")
		}
	}
}

func TestHelperStorageUpgradePreservesSubmittedWorkflow(t *testing.T) {
	for _, policy := range []string{"", legacyHelperStoragePolicy, helperStoragePolicy} {
		t.Run(policy, func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			runningSeedPods(kube)
			client := &createRetryWorkflowClient{}
			c, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var seeds []seedExecCall
			c.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
			r := isolationControllerRequest(t, "sidecars")
			r.SourceDir = writeCheckout(t)
			if err := recoverHelperStorage(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{helperStorageAnnotation: policy}}}, &r); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Create(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			before := client.stored.DeepCopy()
			if before.Annotations[helperStorageAnnotation] != policy {
				t.Fatal("not a legacy fixture")
			}
			seedCount := len(seeds)
			r.legacyHelperStorage, r.legacyProfileHelperResources = false, false
			if _, err := c.Create(t.Context(), r); err != nil {
				t.Fatal("upgrade changed submitted workflow", err)
			}
			if !reflect.DeepEqual(before, client.stored) || len(seeds) != seedCount || len(client.posts) != 1 {
				t.Fatal("recovery rewrote workflow or source")
			}
			client.stored.Annotations[helperStorageAnnotation] = "unknown-policy"
			if _, err := c.Create(t.Context(), r); err == nil || !strings.Contains(err.Error(), "unknown helper-storage") {
				t.Fatalf("unknown policy accepted: %v", err)
			}
		})
	}
}

func TestHelperStorageNewBuildOverridesAuthoredPolicy(t *testing.T) {
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	source := strings.Replace(resourceBoundsSource(""), "    oberth.ci/size: XL", "    oberth.ci/size: XL\n    oberth.ci/helper-storage: unknown-policy", 1)
	wf, err := Build(config, resourceBoundsRequest(source))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
		t.Fatal("authored policy selected helper resources")
	}
}

func TestTrustedHelperStorageRecoveryKeepsExactAttestation(t *testing.T) {
	for _, policy := range []string{"", legacyHelperStoragePolicy, helperStoragePolicy} {
		t.Run(policy, func(t *testing.T) {
			config, request := trustedKVMRequest(t)
			request = prepareTrustedTestRequest(t, config, request)
			if err := recoverHelperStorage(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{helperStorageAnnotation: policy}}}, &request); err != nil {
				t.Fatal(err)
			}
			wf, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			wf.UID = "legacy-workflow"
			wf.Status.StartedAt = metav1.NewTime(request.TrustedKVM.AdmittedAt)
			var leaf *wfv1.Template
			for i := range wf.Spec.Templates {
				if wf.Spec.Templates[i].Name == trustedKVMTemplate {
					leaf = &wf.Spec.Templates[i]
				}
			}
			if leaf == nil {
				t.Fatal("missing trusted leaf")
			}
			pod := trustedKVMMaterializedPod(t, wf, leaf)
			controller, err := NewController(&staticWorkflowClient{workflow: wf}, trustedControllerClient(pod), config)
			if err != nil {
				t.Fatal(err)
			}
			request.legacyHelperStorage = false
			request.legacyProfileHelperResources = false
			_, recovered, err := controller.trustedKVMWorkflow(t.Context(), request, false)
			if err != nil {
				t.Fatal("legacy trusted workflow cannot be reconstructed", err)
			}
			if !reflect.DeepEqual(leaf, recovered) {
				t.Fatal("legacy trusted spec changed")
			}
			wait := &pod.Spec.Containers[0]
			if err := verifyTrustedKVMHelpers(pod, wait, controller.config, wf, recovered); err != nil {
				t.Fatal(err)
			}
			pod.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{}
			if err := verifyTrustedKVMHelpers(pod, wait, controller.config, wf, recovered); err == nil {
				t.Fatal("mixed old/new waiter budget bypassed exact attestation")
			}
		})
	}
}
