package argojob

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func resourceBoundsSource(resources string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: XL
spec:
  entrypoint: step-abc12345
  activeDeadlineSeconds: 3600
  templates:
  - name: step-abc12345
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      command: [/bin/true]
` + resources
}
func resourceBoundsRequest(source string) Request {
	r := testRequest(pipeline.TriggerCI, source)
	r.Repo = "repo-01"
	r.UpstreamName = "repo-01"
	r.UpstreamOrg = "repo-01"
	r.ApprovedSecrets = map[string]bool{}
	return r
}

func assertBoundedControlContainer(t *testing.T, container corev1.Container) {
	t.Helper()
	assertControlContainer(t, container, true)
}

// A helper of a step without its own ephemeral-storage limit reserves
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

func TestCurrentHelperStorageRecoveryPreservesSubmittedWorkflow(t *testing.T) {
	for _, policy := range []string{helperStoragePolicy} {
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
			if _, err := c.Create(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			before := client.stored.DeepCopy()
			if before.Annotations[helperStorageAnnotation] != policy {
				t.Fatal("not the current helper policy")
			}
			seedCount := len(seeds)
			if _, err := c.Create(t.Context(), r); err != nil {
				t.Fatal("upgrade changed submitted workflow", err)
			}
			if !reflect.DeepEqual(before, client.stored) || len(seeds) != seedCount || len(client.posts) != 1 {
				t.Fatal("recovery rewrote workflow or source")
			}
			client.stored.Annotations[helperStorageAnnotation] = "unknown-policy"
			if _, err := c.Create(t.Context(), r); err == nil || !strings.Contains(err.Error(), "unsupported helper-storage") {
				t.Fatalf("unknown policy accepted: %v", err)
			}
		})
	}
}

func TestHelperStorageNewBuildRefusesAuthoredPolicy(t *testing.T) {
	config := testConfig()
	config.RunnerImagePrefixes = []string{"example.invalid/"}
	source := strings.Replace(resourceBoundsSource(""), "    example.com/tier: XL", "    example.com/tier: XL\n    oberth.ci/helper-storage: unknown-policy", 1)
	if _, err := Build(config, resourceBoundsRequest(source)); err == nil || !strings.Contains(err.Error(), "metadata.annotations") {
		t.Fatalf("authored helper policy admitted: %v", err)
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

// Argo v4.0.8 merges spec.templateDefaults into each template before it builds
// the Pod: main fields merge, container-set lists replace, and sidecars and
// init containers merge by name, so default-only entries run too. The helper
// policy decides "declared" from that merged Pod.
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
		want      bool
		container string // the step container that carries the limit
	}{
		{name: "container", leaf: wfv1.Template{Container: withLimit.DeepCopy()}, want: true, container: "main"},
		{name: "container-request-only", leaf: wfv1.Template{Container: &corev1.Container{Resources: requestOnly}}},
		{name: "script", leaf: wfv1.Template{Script: &wfv1.ScriptTemplate{Container: withLimit}}, want: true, container: "main"},
		{name: "containerset", leaf: wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "a"}}, {Container: withLimit}}}}, want: true, container: "main"},
		{name: "sidecar", leaf: wfv1.Template{Container: &corev1.Container{}, Sidecars: []wfv1.UserContainer{named("proxy", limit, nil)}}, want: true, container: "proxy"},
		{name: "template-defaults", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Container: &corev1.Container{}}, want: true, container: "main"},
		{name: "template-defaults-other-kind", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Script: &wfv1.ScriptTemplate{}}},
		// An explicit zero overrides the default in the merged map.
		{name: "template-defaults-zero-override", defaults: &wfv1.Template{Container: withLimit.DeepCopy()}, leaf: wfv1.Template{Container: &corev1.Container{Resources: zero}}},
		// Container-set lists replace defaults, so the default limit never runs.
		{name: "containerset-defaults-only", defaults: &wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: withLimit}}}}, leaf: wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main"}}}}}},
		{name: "sidecar-inherits-by-name", defaults: &wfv1.Template{Sidecars: []wfv1.UserContainer{named("proxy", limit, nil)}}, leaf: wfv1.Template{Container: &corev1.Container{}, Sidecars: []wfv1.UserContainer{named("proxy", requestOnly, nil)}}, want: true, container: "proxy"},
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
			limits := stepStorageLimits(wf, &tc.leaf)
			if _, ok := limits[tc.container]; ok != (tc.container != "") || len(limits) > 1 {
				t.Fatalf("step storage limits %v, want only %q", limits, tc.container)
			}
			// Each policy limits helpers exactly when its own decision says so.
			for policy, declared := range map[string]bool{helperStoragePolicy: tc.want} {
				wf.Annotations[helperStorageAnnotation] = policy
				_, limited := helperResources(wf, &tc.leaf).Limits[corev1.ResourceEphemeralStorage]
				if limited != declared {
					t.Fatalf("%s helper limit=%t, want %t", policy, limited, declared)
				}
			}
		})
	}
}

func TestRemovedExecutionFormatsRefusedWithoutReseeding(t *testing.T) {
	config := seedTestConfig()
	kube := fake.NewClientset()
	runningSeedPods(kube)
	client := &createRetryWorkflowClient{}
	controller, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	var seeds []seedExecCall
	controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
	request := isolationControllerRequest(t, "sidecars")
	request.SourceDir = writeCheckout(t)
	if _, err := controller.Create(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	current := client.stored.DeepCopy()
	seedCount := len(seeds)
	for _, policy := range []string{"", "argo-256Mi-512Mi-v1", "argo-256Mi-512Mi-v2", "argo-256Mi-512Mi-v3", "unknown-policy"} {
		t.Run(policy, func(t *testing.T) {
			client.stored = current.DeepCopy()
			client.stored.Annotations[helperStorageAnnotation] = policy
			if _, err := controller.Create(t.Context(), request); err == nil {
				t.Fatal("removed execution format recovered")
			}
			if len(seeds) != seedCount || len(client.posts) != 1 {
				t.Fatal("refused recovery reseeded or recreated the Workflow")
			}
			adopted, binding := adoptionWorkflow()
			adopted.Annotations[helperStorageAnnotation] = policy
			observer := &Controller{workflows: &staticWorkflowClient{workflow: adopted}, config: Config{Namespace: adopted.Namespace}}
			if _, found, err := observer.Adopt(t.Context(), binding); found || !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("removed format adopted: %v %v", found, err)
			}
		})
	}
}
