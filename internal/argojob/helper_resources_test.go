package argojob

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func assertBoundedControlContainer(t *testing.T, container corev1.Container) {
	t.Helper()
	assertControlContainer(t, container, true)
}

// A v3 helper of a step without its own ephemeral-storage limit reserves
// storage but must not add a limit: the kubelet Pod total would become a cap.
func assertControlContainer(t *testing.T, container corev1.Container, storageLimit bool) {
	t.Helper()
	for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		request := container.Resources.Requests[key]
		limit, limited := container.Resources.Limits[key]
		if key == corev1.ResourceEphemeralStorage && !storageLimit {
			if request.Sign() <= 0 || limited {
				t.Errorf("helper %s requires a positive %s request and no limit", container.Name, key)
			}
			continue
		}
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
	declared := corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}}
	cases = append(cases, struct {
		name     string
		leaf     wfv1.Template
		wantWait bool
		wantMain bool
	}{name: "container-declared", leaf: wfv1.Template{Container: declared.DeepCopy()}, wantWait: true})
	for _, policy := range []string{cappedHelperStoragePolicy, stepLimitHelperStoragePolicy, helperStoragePolicy} {
		for _, profile := range []string{"", "verified-fixture"} {
			for _, tc := range cases {
				t.Run(policy+"/"+profile+"/"+tc.name, func(t *testing.T) {
					leaf := tc.leaf
					leaf.Name = "step"
					workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{nonrootProfileAnnotation: profile, helperStorageAnnotation: policy}}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf}}}
					if err := projectExecutorTokens(workflow, testConfig(), true); err != nil {
						t.Fatal(err)
					}
					var patch corev1.PodSpec
					if err := json.Unmarshal([]byte(workflow.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
						t.Fatal(err)
					}
					// v2 limits every helper; v3/v4 only when the step declares a limit.
					limited := policy == cappedHelperStoragePolicy || tc.name == "container-declared"
					seen := map[string]bool{}
					for _, container := range append(patch.InitContainers, patch.Containers...) {
						assertControlContainer(t, container, limited)
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
	for _, policy := range []string{"", legacyHelperStoragePolicy, cappedHelperStoragePolicy, stepLimitHelperStoragePolicy, helperStoragePolicy} {
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
			r.legacyHelperStorage, r.legacyProfileHelperResources, r.legacyHelperStorageLimits, r.legacyStepLimitHelperStorage = false, false, false, false
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
	source := strings.Replace(resourceBoundsSource(""), "    example.com/tier: XL", "    example.com/tier: XL\n    oberth.ci/helper-storage: unknown-policy", 1)
	wf, err := Build(config, resourceBoundsRequest(source))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
		t.Fatal("authored policy selected helper resources")
	}
}

// podStorageLimit mirrors the kubelet's PodLimits for ephemeral-storage: the
// sum of declared regular-container limits (restartable init containers
// included), raised to the largest one-shot init container's limit. Zero
// means the kubelet applies no Pod-level ephemeral-storage eviction.
func podStorageLimit(spec corev1.PodSpec) int64 {
	var sum, restartable, initMax int64
	for _, c := range spec.Containers {
		sum += c.Resources.Limits.StorageEphemeral().Value()
	}
	for _, c := range spec.InitContainers {
		limit := c.Resources.Limits.StorageEphemeral().Value()
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sum += limit
			restartable += limit
			limit = restartable
		} else {
			limit += restartable
		}
		initMax = max(initMax, limit)
	}
	return max(sum, initMax)
}

// #1491: v2 gave every helper a 512Mi limit, so a step without its own limit
// ran in a Pod capped at 512Mi in total, including its /cache and /tmp
// emptyDirs. v3 must leave such a Pod uncapped and keep the declared sum.
func TestHelperStorageAddsNoPodCapToUndeclaredStep(t *testing.T) {
	undeclared := "      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi}\n"
	declared := "      resources:\n        requests: {cpu: 125m, memory: 128Mi, ephemeral-storage: 64Mi}\n        limits: {memory: 256Mi, ephemeral-storage: 128Mi}\n"
	cases := []struct {
		name, resources, policy string
		wantLimit               int64
	}{
		{"undeclared/v4", undeclared, helperStoragePolicy, 0},
		{"declared/v4", declared, helperStoragePolicy, (128 + 512) << 20},
		{"undeclared/v3", undeclared, stepLimitHelperStoragePolicy, 0},
		{"declared/v3", declared, stepLimitHelperStoragePolicy, (128 + 512) << 20},
		// Recovered v2 Workflows keep their submitted shape, including the cap.
		{"undeclared/v2", undeclared, cappedHelperStoragePolicy, 512 << 20},
		{"declared/v2", declared, cappedHelperStoragePolicy, (128 + 512) << 20},
	}
	for _, placement := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("placement=%t/%s", placement, tc.name), func(t *testing.T) {
				config := testConfig()
				if !placement {
					config.ExecutionPlacement = nil
				}
				config.RunnerImagePrefixes = []string{"example.invalid/"}
				request := resourceBoundsRequest(resourceBoundsSource(tc.resources))
				if err := recoverHelperStorage(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{helperStorageAnnotation: tc.policy}}}, &request); err != nil {
					t.Fatal(err)
				}
				wf, err := Build(config, request)
				if err != nil {
					t.Fatal(err)
				}
				if wf.Annotations[helperStorageAnnotation] != tc.policy {
					t.Fatalf("policy %q, want %q", wf.Annotations[helperStorageAnnotation], tc.policy)
				}
				leaf := wf.Spec.Templates[0]
				main := *leaf.Container.DeepCopy()
				main.Name = "main"
				after := applyCredentialWaitPatch(t, credentialWaitControllerShape(main), leaf.PodSpecPatch)
				if got := podStorageLimit(after); got != tc.wantLimit {
					t.Fatalf("Pod ephemeral-storage limit %d, want %d", got, tc.wantLimit)
				}
				// Helpers keep their scheduling reservation in every case.
				var waitRequest int64
				helpers := 0
				for _, c := range append(after.InitContainers, after.Containers...) {
					if c.Name == "main" {
						continue
					}
					helpers++
					assertControlContainer(t, c, tc.wantLimit != 0)
					if c.Name == "wait" {
						waitRequest = c.Resources.Requests.StorageEphemeral().Value()
					}
				}
				if helpers != 3 || waitRequest != 256<<20 {
					t.Fatalf("helpers %d with wait request %d", helpers, waitRequest)
				}
			})
		}
	}
}

// Argo v4.0.8 merges spec.templateDefaults into each template before it builds
// the Pod: main fields merge, container-set lists replace, and sidecars and
// init containers merge by name, so default-only entries run too. v4 decides
// "declared" from that merged Pod. v3 keeps its historical decision so adopted
// v3 Workflows rebuild exactly; the rows where they differ are marked.
func TestStepDeclaresStorageLimitCoversStepContainers(t *testing.T) {
	limit := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}
	requestOnly := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}
	zero := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("0")}}
	always := corev1.ContainerRestartPolicyAlways
	withLimit := corev1.Container{Name: "main", Resources: limit}
	named := func(name string, resources corev1.ResourceRequirements, restart *corev1.ContainerRestartPolicy) wfv1.UserContainer {
		return wfv1.UserContainer{Container: corev1.Container{Name: name, Resources: resources, RestartPolicy: restart}}
	}
	cases := []struct {
		name      string
		defaults  *wfv1.Template
		leaf      wfv1.Template
		want, v3  bool
		container string // the step container that carries the limit
	}{
		{name: "container", leaf: wfv1.Template{Container: withLimit.DeepCopy()}, want: true, v3: true, container: "main"},
		{name: "container-request-only", leaf: wfv1.Template{Container: &corev1.Container{Resources: requestOnly}}},
		{name: "script", leaf: wfv1.Template{Script: &wfv1.ScriptTemplate{Container: withLimit}}, want: true, v3: true, container: "main"},
		{name: "containerset", leaf: wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "a"}}, {Container: withLimit}}}}, want: true, v3: true, container: "main"},
		{name: "sidecar", leaf: wfv1.Template{Container: &corev1.Container{}, Sidecars: []wfv1.UserContainer{named("proxy", limit, nil)}}, want: true, v3: true, container: "proxy"},
		{name: "template-defaults", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Container: &corev1.Container{}}, want: true, v3: true, container: "main"},
		{name: "template-defaults-other-kind", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Script: &wfv1.ScriptTemplate{}}},
		// An explicit zero overrides the default in the merged map.
		{name: "template-defaults-zero-override", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Container: &corev1.Container{Resources: zero}}, v3: true},
		// Container-set lists replace defaults, so the default limit never runs.
		{name: "containerset-defaults-only", defaults: &wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: withLimit}}}}, leaf: wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main"}}}}}, v3: true},
		{name: "sidecar-inherits-by-name", defaults: &wfv1.Template{Sidecars: []wfv1.UserContainer{named("proxy", limit, nil)}}, leaf: wfv1.Template{Container: &corev1.Container{}, Sidecars: []wfv1.UserContainer{named("proxy", requestOnly, nil)}}, want: true, v3: true, container: "proxy"},
		{name: "sidecar-defaults-only", defaults: &wfv1.Template{Sidecars: []wfv1.UserContainer{named("proxy", limit, nil)}}, leaf: wfv1.Template{Container: &corev1.Container{}}, want: true, container: "proxy"},
		{name: "native-sidecar", leaf: wfv1.Template{Container: &corev1.Container{}, InitContainers: []wfv1.UserContainer{named("proxy", limit, &always)}}, want: true, container: "proxy"},
		{name: "native-sidecar-defaults-only", defaults: &wfv1.Template{InitContainers: []wfv1.UserContainer{named("proxy", limit, &always)}}, leaf: wfv1.Template{Container: &corev1.Container{}}, want: true, container: "proxy"},
		{name: "native-sidecar-inherits-restart", defaults: &wfv1.Template{InitContainers: []wfv1.UserContainer{named("proxy", requestOnly, &always)}}, leaf: wfv1.Template{Container: &corev1.Container{}, InitContainers: []wfv1.UserContainer{named("proxy", limit, nil)}}, want: true, container: "proxy"},
		// One-shot init containers enter the kubelet total by maximum and keep their own effect.
		{name: "init-only", leaf: wfv1.Template{Container: &corev1.Container{}, InitContainers: []wfv1.UserContainer{named("setup", limit, nil)}}},
		{name: "init-defaults-only", defaults: &wfv1.Template{InitContainers: []wfv1.UserContainer{named("setup", limit, nil)}}, leaf: wfv1.Template{Container: &corev1.Container{}}},
		{name: "resource", leaf: wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}, Spec: wfv1.WorkflowSpec{TemplateDefaults: tc.defaults}}
			if got := stepDeclaresStorageLimit(wf, &tc.leaf); got != tc.want {
				t.Fatalf("declared=%t, want %t", got, tc.want)
			}
			if got := stepDeclaresStorageLimitV3(wf, &tc.leaf); got != tc.v3 {
				t.Fatalf("v3 declared=%t, want %t", got, tc.v3)
			}
			limits := stepStorageLimits(wf, &tc.leaf)
			if _, ok := limits[tc.container]; ok != (tc.container != "") || len(limits) > 1 {
				t.Fatalf("step storage limits %v, want only %q", limits, tc.container)
			}
			// Each policy limits helpers exactly when its own decision says so.
			for policy, declared := range map[string]bool{helperStoragePolicy: tc.want, stepLimitHelperStoragePolicy: tc.v3, cappedHelperStoragePolicy: true} {
				wf.Annotations[helperStorageAnnotation] = policy
				_, limited := helperResources(wf, &tc.leaf, true).Limits[corev1.ResourceEphemeralStorage]
				if limited != declared {
					t.Fatalf("%s helper limit=%t, want %t", policy, limited, declared)
				}
			}
		})
	}
}
