package argojob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestExecutorIdentityRejectsWaitStartHookDrift(t *testing.T) {
	for _, shape := range []string{"container", "resource-with-logs"} {
		for _, mutation := range []string{"missing", "zero", "longer", "exec", "pre-stop", "extra-action"} {
			t.Run(shape+"/"+mutation, func(t *testing.T) {
				c, _, pod, _ := executorRetryFixture(t)
				pod.Spec = executorEnvironmentPod(t, shape, "wait", nil)
				if err := validateExecutorTokenTarget(pod, c.config.SourceSeedImage, shape != "container"); err != nil {
					t.Fatal("server patch rejected", err)
				}
				for i := range pod.Spec.Containers {
					container := &pod.Spec.Containers[i]
					if container.Name != "wait" {
						continue
					}
					switch mutation {
					case "missing":
						container.Lifecycle = nil
					case "zero":
						container.Lifecycle.PostStart.Sleep.Seconds = 0
					case "longer":
						container.Lifecycle.PostStart.Sleep.Seconds = 2
					case "exec":
						container.Lifecycle.PostStart = &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sleep", "1"}}}
					case "pre-stop":
						container.Lifecycle.PreStop = container.Lifecycle.PostStart
						container.Lifecycle.PostStart = nil
					case "extra-action":
						container.Lifecycle.PostStart.HTTPGet = &corev1.HTTPGetAction{Path: "/"}
					}
				}
				if err := validateExecutorTokenTarget(pod, c.config.SourceSeedImage, shape != "container"); err == nil {
					t.Fatal("modified wait start hook accepted")
				}
			})
		}
	}
}

func TestExecutorWaitStartHookPatch(t *testing.T) {
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "step"}}}}}
	if err := projectExecutorTokens(wf, config); err != nil {
		t.Fatal(err)
	}
	var patch corev1.PodSpec
	if err := json.Unmarshal([]byte(wf.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
		t.Fatal(err)
	}
	for _, c := range patch.Containers {
		if c.Name == "wait" && c.Lifecycle != nil && c.Lifecycle.PostStart != nil && c.Lifecycle.PostStart.Sleep != nil && c.Lifecycle.PostStart.Sleep.Seconds == 1 {
			return
		}
	}
	t.Fatal("wait lacks native one-second postStart sleep")
}

// waitStartHookPatch is the exact serialized form the server patch gives wait.
const waitStartHookPatch = `"lifecycle":{"postStart":{"sleep":{"seconds":1}}},`

// withoutWaitStartHookPatch requires the current server patch to carry the
// wait start hook exactly once per wait entry and removes it, so captures that
// predate the hook still compare on every other field.
func withoutWaitStartHookPatch(t *testing.T, patch string) string {
	t.Helper()
	if !strings.Contains(patch, `"name":"wait"`) {
		return patch
	}
	if strings.Count(patch, waitStartHookPatch) != 1 {
		t.Fatalf("server patch lacks the exact wait start hook: %s", patch)
	}
	return strings.Replace(patch, waitStartHookPatch, "", 1)
}

func withoutWorkflowWaitStartHook(t *testing.T, wf *wfv1.Workflow) {
	t.Helper()
	for i := range wf.Spec.Templates {
		wf.Spec.Templates[i].PodSpecPatch = withoutWaitStartHookPatch(t, wf.Spec.Templates[i].PodSpecPatch)
	}
}

// withoutCurrentWaitStartHook strips the hook from a Pod built by the current
// server patch: wait's lifecycle and the hook inside ARGO_TEMPLATE's patch.
func withoutCurrentWaitStartHook(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	expected := &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: executorWaitStartDelaySeconds}}}
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			c := &containers[i]
			if c.Name == "wait" {
				if !reflect.DeepEqual(c.Lifecycle, expected) {
					t.Fatalf("current wait lacks the exact start hook: %+v", c.Lifecycle)
				}
				c.Lifecycle = nil
			}
			for j := range c.Env {
				if c.Env[j].Name != "ARGO_TEMPLATE" {
					continue
				}
				var template wfv1.Template
				if err := json.Unmarshal([]byte(c.Env[j].Value), &template); err != nil {
					t.Fatal(err)
				}
				template.PodSpecPatch = withoutWaitStartHookPatch(t, template.PodSpecPatch)
				body, err := json.Marshal(template)
				if err != nil {
					t.Fatal(err)
				}
				c.Env[j].Value = string(body)
			}
		}
	}
}

// withCapturedWaitStartHook models only the new server patch on an Argo Pod
// captured before the wait start hook existed.
func withCapturedWaitStartHook(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "wait" {
			pod.Spec.Containers[i].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: executorWaitStartDelaySeconds}}}
		}
	}
}
