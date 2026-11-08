package argojob

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// assistDocument has a deadline-bearing leaf, a leaf without its own
// deadline, a leaf with an authored retryStrategy and the DAG that runs them.
const assistDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      dag:
        tasks:
          - name: test
            template: test
          - name: lint
            template: lint
            dependencies: [test]
          - name: flaky
            template: flaky
    - name: test
      activeDeadlineSeconds: 600
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [go, test, ./...]
        workingDir: /work/src
        resources:
          requests: {cpu: 500m, memory: 512Mi}
          limits: {cpu: "1", memory: 512Mi}
    - name: lint
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [go, vet, ./...]
        resources:
          requests: {cpu: 500m, memory: 128Mi}
          limits: {cpu: "1", memory: 128Mi}
    - name: flaky
      retryStrategy:
        limit: 2
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [go, run, ./flaky]
        resources:
          requests: {cpu: 500m, memory: 128Mi}
          limits: {cpu: "1", memory: 128Mi}
`

func assistConfig() Config {
	config := cachingConfig()
	config.Assist = AssistConfig{Enabled: true, HoldTTL: 20 * time.Minute}
	return config
}

func assistRequest(trigger pipeline.Trigger, source string) Request {
	request := testRequest(trigger, source)
	request.Assist = true
	request.SourceVolume = SourceVolume{ClaimName: "source-claim", SubPath: "source", BinarySubPath: "bin"}
	return request
}

func TestBuildWithoutAssistInjectsNoHolds(t *testing.T) {
	cases := map[string]func(*Config, *Request){
		"policy disabled":   func(config *Config, _ *Request) { config.Assist.Enabled = false },
		"run did not ask":   func(_ *Config, request *Request) { request.Assist = false },
		"release tier":      func(_ *Config, request *Request) { request.Trigger = pipeline.TriggerRelease },
		"schedule trigger":  func(_ *Config, request *Request) { request.RunTrigger = argoworkflow.RunTriggerSchedule },
		"promotion trigger": func(_ *Config, request *Request) { request.RunTrigger = argoworkflow.RunTriggerPromotion },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config, request := assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument)
			mutate(&config, &request)
			wf, err := Build(config, request)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if _, ok := wf.Annotations[AssistWorkflowAnnotation]; ok {
				t.Fatalf("workflow marked assisted: %v", wf.Annotations)
			}
			if got := *wf.Spec.ActiveDeadlineSeconds; got != 3600 {
				t.Fatalf("workflow deadline = %d, want the declared 3600", got)
			}
			for _, template := range wf.Spec.Templates {
				if _, ok := template.Metadata.Annotations[AssistHoldAnnotation]; ok {
					t.Fatalf("template %q marked for hold", template.Name)
				}
				if template.Name != "flaky" && template.RetryStrategy != nil {
					t.Fatalf("template %q gained a retryStrategy", template.Name)
				}
				if strings.Contains(template.PodSpecPatch, "{{") {
					t.Fatalf("template %q Pod patch is conditional: %s", template.Name, template.PodSpecPatch)
				}
			}
		})
	}
}

func TestBuildAssistMarksOnlyCredentialFreeLeavesWithoutOwnRetries(t *testing.T) {
	wf, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := wf.Annotations[AssistWorkflowAnnotation]; got != "1200" {
		t.Fatalf("workflow assist annotation = %q, want the 1200-second hold TTL", got)
	}
	if got := *wf.Spec.ActiveDeadlineSeconds; got != 3600+1200 {
		t.Fatalf("workflow deadline = %d, want 3600 + one hold TTL", got)
	}
	// Argo's workflow/common constants are not imported by the server; the
	// expression variables are the documented retry-local names.
	test := templateNamed(t, wf, "test")
	floored := DefaultStepStartupAllowance.Floored(600)
	if want := strconv.FormatInt(floored+1200, 10); test.Metadata.Annotations[AssistHoldAnnotation] != want {
		t.Fatalf("test hold deadline = %q, want the floored step budget %d + 1200", test.Metadata.Annotations[AssistHoldAnnotation], floored)
	}
	if test.RetryStrategy == nil || test.RetryStrategy.Limit.String() != "1" || test.RetryStrategy.RetryPolicy != wfv1.RetryPolicyOnFailure || test.RetryStrategy.Expression != assistRetryExpression {
		t.Fatalf("test retryStrategy = %+v", test.RetryStrategy)
	}
	if !strings.HasPrefix(test.PodSpecPatch, "{{= "+assistPatchCondition+" ? '") {
		t.Fatalf("test Pod patch is not the conditional expression: %s", test.PodSpecPatch)
	}
	lint := templateNamed(t, wf, "lint")
	if got := lint.Metadata.Annotations[AssistHoldAnnotation]; got != "4800" {
		t.Fatalf("lint hold deadline = %q, want the 3600 workflow budget + 1200", got)
	}
	if lint.RetryStrategy == nil {
		t.Fatal("lint (no own deadline) is hold-eligible and gained no retryStrategy")
	}
	flaky := templateNamed(t, wf, "flaky")
	if _, ok := flaky.Metadata.Annotations[AssistHoldAnnotation]; ok || flaky.RetryStrategy.Limit.String() != "2" || flaky.RetryStrategy.Expression != "" {
		t.Fatalf("authored retryStrategy was changed: %+v %v", flaky.RetryStrategy, flaky.Metadata.Annotations)
	}
	if strings.Contains(flaky.PodSpecPatch, "{{") {
		t.Fatalf("flaky Pod patch became conditional: %s", flaky.PodSpecPatch)
	}
	main := templateNamed(t, wf, "main")
	if main.RetryStrategy != nil || main.Metadata.Annotations[AssistHoldAnnotation] != "" {
		t.Fatal("the DAG template is not a Pod and must not hold")
	}
}

func TestBuildAssistSkipsCredentialedWorkflows(t *testing.T) {
	document := strings.Replace(assistDocument, "oberth.ci/size: M", "oberth.ci/size: M\n    oberth.ci/secret-paths: oberth/upstream/acme/oberth/token", 1)
	request := assistRequest(pipeline.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/acme/oberth/token": true}
	request.StepGrants = map[string]map[string]bool{"oberth/upstream/acme/oberth/token": {"*": true}}
	wf, err := Build(assistConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := wf.Annotations[AssistWorkflowAnnotation]; ok {
		t.Fatal("a credentialed workflow must never hold")
	}
	for _, template := range wf.Spec.Templates {
		if template.Name != "flaky" && template.RetryStrategy != nil {
			t.Fatalf("template %q gained a retryStrategy in a credentialed workflow", template.Name)
		}
	}
}

func TestAssistHoldEligibilityRefusesIdentityAndLaunchers(t *testing.T) {
	wf, err := argoworkflow.Decode([]byte(assistDocument))
	if err != nil {
		t.Fatal(err)
	}
	leaf := templateNamed(t, wf, "lint")
	if !assistHoldEligible(leaf, "", nil) {
		t.Fatal("plain leaf ineligible")
	}
	if assistHoldEligible(leaf, "", map[string]bool{"lint": true}) {
		t.Fatal("a nonroot launcher leaf forbids exec and must not hold")
	}
	leaf.ServiceAccountName = "oberth-argo-pipeline"
	if !assistHoldEligible(leaf, "oberth-argo-pipeline", nil) {
		t.Fatal("the forced pipeline identity is not a step identity")
	}
	if assistHoldEligible(leaf, "oberth-argo-other", nil) {
		t.Fatal("a per-step identity must not hold")
	}
	leaf.ServiceAccountName = ""
	leaf.Metadata.Annotations = map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "publisher"}
	if assistHoldEligible(leaf, "", nil) {
		t.Fatal("a release WIF leaf must not hold")
	}
	leaf.Metadata.Annotations = nil
	leaf.Container.Command = []string{"/run/oberth/bin/oberth", "secretstore", "exec", "--", "make"}
	if assistHoldEligible(leaf, "", nil) {
		t.Fatal("a credential-chain leaf must not hold")
	}
	leaf.Container.Command = []string{"make"}
	leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: ReleaseTokenVolumeName, MountPath: ReleaseTokenMountPath}}
	if assistHoldEligible(leaf, "", nil) {
		t.Fatal("a token-mounting leaf must not hold")
	}
	leaf.Container.VolumeMounts = nil
	automount := true
	leaf.AutomountServiceAccountToken = &automount
	if assistHoldEligible(leaf, "", nil) {
		t.Fatal("an automounted token must not hold")
	}
	if assistHoldEligible(templateNamed(t, wf, "main"), "", nil) || assistHoldEligible(templateNamed(t, wf, "flaky"), "", nil) {
		t.Fatal("DAG and authored-retry templates must not hold")
	}
}

func TestBuildRefusesRepositoryAssistAnnotations(t *testing.T) {
	document := strings.Replace(assistDocument, "    - name: test\n      activeDeadlineSeconds: 600\n", "    - name: test\n      activeDeadlineSeconds: 600\n      metadata:\n        annotations:\n          oberth.ci/assist-hold: \"99999\"\n", 1)
	if _, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, document)); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") {
		t.Fatalf("repository hold annotation admitted: %v", err)
	}
	authored := strings.Replace(assistDocument, "oberth.ci/size: M", "oberth.ci/size: M\n    oberth.ci/assist: \"1\"", 1)
	request := assistRequest(pipeline.TriggerCI, authored)
	request.Assist = false
	wf, err := Build(assistConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := wf.Annotations[AssistWorkflowAnnotation]; ok {
		t.Fatal("an authored workflow assist annotation survived Build")
	}
}

// TestAssistPatchResolvesWithArgoTemplateEngine substitutes the conditional
// Pod patch exactly as the Argo v4.0.8 controller does before it creates a
// retry attempt's Pod (operator.go SubstituteParams with the retry-local
// variables), then applies it with the same strategic merge
// (workflow/util.ApplyPodSpecPatch). Attempt 1 and a deadline-killed
// attempt keep the base patch; a failed command parks the retry.
func TestAssistPatchResolvesWithArgoTemplateEngine(t *testing.T) {
	wf, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	test := templateNamed(t, wf, "test")
	encoded, err := json.Marshal(test)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(retries, exitCode string) *corev1.PodSpec {
		t.Helper()
		params := map[string]any{"retries": retries, "lastRetry.exitCode": exitCode, "lastRetry.status": "Failed", "lastRetry.duration": "10", "lastRetry.message": ""}
		ctx := argologging.WithLogger(context.Background(), argologging.NewSlogLogger(argologging.Info, argologging.Text))
		replaced, err := argotemplate.Replace(ctx, string(encoded), params, true)
		if err != nil {
			t.Fatalf("argo template replace: %v", err)
		}
		var resolved wfv1.Template
		if err := json.Unmarshal([]byte(replaced), &resolved); err != nil {
			t.Fatalf("resolved template: %v", err)
		}
		if strings.Contains(resolved.PodSpecPatch, "{{") {
			t.Fatalf("patch left unresolved: %s", resolved.PodSpecPatch)
		}
		var patch corev1.PodSpec
		if err := json.Unmarshal([]byte(resolved.PodSpecPatch), &patch); err != nil {
			t.Fatalf("resolved patch is not a PodSpec: %v: %s", err, resolved.PodSpecPatch)
		}
		deadline := int64(DefaultStepStartupAllowance.Floored(600))
		pod := corev1.PodSpec{ActiveDeadlineSeconds: &deadline, Containers: []corev1.Container{*test.Container, {Name: "wait"}}}
		pod.Containers[0].Name = mainContainerName
		podJSON, _ := json.Marshal(pod)
		merged, err := strategicpatch.StrategicMergePatch(podJSON, []byte(resolved.PodSpecPatch), corev1.PodSpec{})
		if err != nil {
			t.Fatalf("strategic merge: %v", err)
		}
		var result corev1.PodSpec
		if err := json.Unmarshal(merged, &result); err != nil {
			t.Fatal(err)
		}
		return &result
	}
	mainOf := func(spec *corev1.PodSpec) *corev1.Container {
		for index := range spec.Containers {
			if spec.Containers[index].Name == mainContainerName {
				return &spec.Containers[index]
			}
		}
		t.Fatal("main container missing")
		return nil
	}
	mountReadOnly := func(c *corev1.Container, mountPath string) (bool, bool) {
		for _, m := range c.VolumeMounts {
			if m.MountPath == mountPath {
				return m.ReadOnly, true
			}
		}
		return false, false
	}

	first := resolve("0", "0")
	if env := containerEnv(mainOf(first)); env[assistPauseEnv] != "" || env[AssistHoldEnv] != "" {
		t.Fatalf("attempt 1 is paused: %v", env)
	}
	if got := *first.ActiveDeadlineSeconds; got != DefaultStepStartupAllowance.Floored(600) {
		t.Fatalf("attempt 1 deadline = %d, want the floored step deadline", got)
	}
	if readOnly, ok := mountReadOnly(mainOf(first), SourceMountPath); !ok || !readOnly {
		t.Fatal("attempt 1 lost its read-only source mount")
	}

	killed := resolve("1", "-1")
	if env := containerEnv(mainOf(killed)); env[assistPauseEnv] != "" {
		t.Fatal("a retry after a deadline kill (no exit code) must not park")
	}

	held := resolve("1", "2")
	main := mainOf(held)
	env := containerEnv(main)
	if env[assistPauseEnv] != "true" || env[AssistHoldEnv] != "1" {
		t.Fatalf("held attempt is not paused: %v", env)
	}
	if env["GOCACHE"] != AssistScratchCachePath+"/gobuild" || env["GOMODCACHE"] != AssistScratchCachePath+"/gomod" || env["OBERTH_CACHE_DIR"] != AssistScratchCachePath {
		t.Fatalf("held attempt still writes the shared cache: GOCACHE=%q GOMODCACHE=%q OBERTH_CACHE_DIR=%q", env["GOCACHE"], env["GOMODCACHE"], env["OBERTH_CACHE_DIR"])
	}
	if got := *held.ActiveDeadlineSeconds; got != DefaultStepStartupAllowance.Floored(600)+1200 {
		t.Fatalf("held attempt deadline = %d, want 600 floored (%d) + 1200", got, DefaultStepStartupAllowance.Floored(600))
	}
	if readOnly, ok := mountReadOnly(main, SourceMountPath); !ok || readOnly {
		t.Fatal("held attempt source mount is not writable")
	}
	if readOnly, ok := mountReadOnly(main, CacheMountPath); !ok || !readOnly {
		t.Fatal("held attempt shared cache mount is not read-only")
	}
	if held.NodeSelector[corev1.LabelArchStable] == "" {
		t.Fatal("held attempt with the server binary mounted needs the server's architecture")
	}
	names := []string{held.Containers[0].Name, held.Containers[1].Name}
	if len(held.Containers) != 2 || !slices.Contains(names, "wait") || !slices.Contains(names, mainContainerName) {
		t.Fatalf("containers changed shape: %v", names)
	}
	for _, c := range main.VolumeMounts {
		if c.MountPath == SourceMountPath && c.SubPath != "source" {
			t.Fatalf("source subPath lost in merge: %+v", c)
		}
	}
	if !AssistHoldPod(&corev1.Pod{Spec: *held}) {
		pod := corev1.Pod{Spec: *held}
		pod.Annotations = map[string]string{AssistHoldAnnotation: "1800"}
		if !AssistHoldPod(&pod) {
			t.Fatal("held pod not recognised")
		}
	}
	if AssistHoldPod(&corev1.Pod{Spec: *first}) {
		t.Fatal("attempt 1 recognised as a hold")
	}
}

func TestAssistConfigValidation(t *testing.T) {
	if err := (AssistConfig{}).Validate(); err != nil {
		t.Fatalf("zero policy: %v", err)
	}
	if err := (AssistConfig{HoldTTL: 500 * time.Millisecond}).Validate(); err == nil {
		t.Fatal("sub-second TTL accepted")
	}
	if err := (AssistConfig{HoldTTL: MaxAssistHoldTTL + time.Second}).Validate(); err == nil {
		t.Fatal("TTL above the ceiling accepted")
	}
	if err := (AssistConfig{MaxConcurrentHolds: MaxAssistConcurrentHolds + 1}).Validate(); err == nil {
		t.Fatal("hold cap above the ceiling accepted")
	}
	effective := (AssistConfig{}).Effective()
	if effective.HoldTTL != DefaultAssistHoldTTL || effective.MaxConcurrentHolds != DefaultAssistMaxConcurrentHolds || effective.ExecTimeout != DefaultAssistExecTimeout {
		t.Fatalf("defaults: %+v", effective)
	}
}
