package argojob

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Evaluate the rendered policy itself, including CEL's absent-field semantics.
// A separate Go approximation would not catch a broken admission expression.
func renderedPodPolicy(t *testing.T) func(*corev1.Pod) bool {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm is required for the pod admission contract")
	}
	cmd := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64), "--show-only", "templates/argo-pod-policy.yaml")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, data)
	}
	var policy admissionv1.ValidatingAdmissionPolicy
	if err := yaml.Unmarshal([]byte(strings.Split(string(data), "\n---")[0]), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionv1.Fail || len(policy.Spec.Validations) != 4 {
		t.Fatalf("incomplete fail-closed policy: %+v", policy.Spec)
	}
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("variables", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		t.Helper()
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	vars := make([]cel.Program, len(policy.Spec.Variables))
	for i, v := range policy.Spec.Variables {
		vars[i] = compile(v.Expression)
	}
	checks := make([]cel.Program, len(policy.Spec.Validations))
	for i, v := range policy.Spec.Validations {
		checks[i] = compile(v.Expression)
	}
	return func(pod *corev1.Pod) bool {
		t.Helper()
		data, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		variables := map[string]any{}
		activation := map[string]any{"object": object, "variables": variables}
		for i, program := range vars {
			value, _, err := program.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			variables[policy.Spec.Variables[i].Name] = value
		}
		for _, program := range checks {
			value, _, err := program.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			if value != types.True {
				return false
			}
		}
		return true
	}
}

func TestPodPolicyRejectsNodeAccess(t *testing.T) {
	accepts := renderedPodPolicy(t)
	baseline := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}, InitContainers: []corev1.Container{{Name: "init"}}, EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}}}
	if !accepts(baseline) {
		t.Fatal("ordinary pod refused")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"host PID":     func(p *corev1.Pod) { p.Spec.HostPID = true },
		"host IPC":     func(p *corev1.Pod) { p.Spec.HostIPC = true },
		"host network": func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"privileged main": func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		},
		"privileged init": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		},
		"privileged ephemeral": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		},
		"main capabilities": func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}
		},
		"init capabilities": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CHOWN"}}}
		},
		"ephemeral capabilities": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := baseline.DeepCopy()
			mutate(p)
			if accepts(p) {
				t.Fatal("unsafe pod admitted")
			}
		})
	}
	for _, path := range []string{"/", "/etc", testCICacheRoot, testCICacheRoot + "-other/repo-123456789abc", testCICacheRoot + "/../repo-123456789abc", testCICacheRoot + "/repo-123456789abc/../../etc", testCICacheRoot + "//repo-123456789abc"} {
		p := baseline.DeepCopy()
		p.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}}
		if accepts(p) {
			t.Errorf("hostPath %q admitted", path)
		}
	}
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		cfg := testConfig()
		cfg.CICacheRoot = testCICacheRoot
		cfg.ReleaseCacheRoot = testReleaseCacheRoot
		volume, _, ok := cacheVolumeAndMount(cfg, testRequest(trigger, admissibleSecurityDocument))
		if !ok {
			t.Fatal("missing cache")
		}
		p := baseline.DeepCopy()
		p.Spec.Volumes = []corev1.Volume{volume}
		if !accepts(p) {
			t.Fatalf("reviewed %s cache refused", trigger)
		}
		p.Spec.Volumes[0].HostPath.Type = ptr.To(corev1.HostPathSocket)
		if accepts(p) {
			t.Fatal("host socket admitted")
		}
	}
}

func TestPodPolicyAcceptsEmittedShapes(t *testing.T) {
	accepts := renderedPodPolicy(t)
	for _, name := range []string{"container-submitted.json", "script-submitted.json"} {
		if !accepts(nonrootControllerFixture(t, name)) {
			t.Fatalf("controller %s refused", name)
		}
	}
	for _, shape := range []string{"container", "script"} {
		wf := nonrootBuiltWithHelperPolicy(t, shape == "script", "128Mi", legacyHelperStoragePolicy)
		pod := nonrootApplyControllerFixture(t, nonrootControllerFixture(t, shape+"-before-patch.json"), wf)
		if !accepts(pod) {
			t.Fatalf("current %s controller shape refused", shape)
		}
	}
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		cfg := testConfig()
		cfg.CICacheRoot = testCICacheRoot
		cfg.ReleaseCacheRoot = testReleaseCacheRoot
		wf, err := Build(cfg, testRequest(trigger, nestedSecurityDocument))
		if err != nil {
			t.Fatal(err)
		}
		walkTemplates(wf, func(template *wfv1.Template) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: wf.Spec.Volumes}}
			templateContainers(template, func(c *corev1.Container) { pod.Spec.Containers = append(pod.Spec.Containers, *c) })
			if len(pod.Spec.Containers) > 0 && !accepts(pod) {
				t.Errorf("%s template %s refused", trigger, template.Name)
			}
		})
	}
	for _, entry := range []struct {
		source, ref string
		trigger     pipeline.Trigger
	}{
		{"build.yaml", "refs/heads/main", pipeline.TriggerCI},
		{"release.yaml", "refs/tags/v0.1.0", pipeline.TriggerRelease},
	} {
		wf, err := buildOberthPipeline(oberthConfig(), Request{
			RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
			Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github",
			Ref: entry.ref, SHA: testSHA, Trigger: entry.trigger,
			Source: loadPipeline(t, entry.source), ApprovedSecrets: oberthReleaseApprovedSecrets(),
		})
		if err != nil {
			t.Fatal(err)
		}
		walkTemplates(wf, func(template *wfv1.Template) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: wf.Spec.Volumes}}
			templateContainers(template, func(c *corev1.Container) { pod.Spec.Containers = append(pod.Spec.Containers, *c) })
			if len(pod.Spec.Containers) > 0 && !accepts(pod) {
				t.Errorf("%s template %s refused", entry.source, template.Name)
			}
		})
	}
	kube := fake.NewClientset()
	seeder := &SourceSeeder{client: kube, config: testConfig()}
	if err := seeder.createSeedPod(t.Context(), "seed", "claim"); err != nil {
		t.Fatal(err)
	}
	pod, err := kube.CoreV1().Pods(testNamespace).Get(t.Context(), "seed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !accepts(pod) {
		t.Fatal("source helper refused")
	}
	controller := &Controller{kube: kube, config: testConfig()}
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: testNamespace, UID: "workflow-uid"}}
	// The fake does not assign UIDs; inspect the created holder even though the
	// ownership guard correctly refuses its empty fake UID.
	_, _ = controller.executorIdentityPod(t.Context(), wf, testExecutorAcct)
	pods, err := kube.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("expected seed and identity helper, got %d", len(pods.Items))
	}
	for i := range pods.Items {
		if !accepts(&pods.Items[i]) {
			t.Fatalf("helper %s refused", pods.Items[i].Name)
		}
	}
}
