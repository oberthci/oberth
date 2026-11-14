package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestExecutorIdentityRejectsWaitStartHookDrift(t *testing.T) {
	for _, shape := range []string{"container", "resource-with-logs"} {
		for _, mutation := range []string{"missing", "zero", "longer", "exec", "pre-stop", "extra-action", "not-first"} {
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
				if mutation == "not-first" {
					// Exact hook, but kubelet would start main before wait.
					containers := pod.Spec.Containers
					if len(containers) < 2 || containers[0].Name != "wait" {
						t.Fatalf("fixture lacks wait first: %d containers", len(containers))
					}
					pod.Spec.Containers = append(append([]corev1.Container{}, containers[1:]...), containers[0])
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

// withCapturedWaitStartHook models only the new server patch on an Argo Pod
// captured before the wait start hook existed.
func withCapturedWaitStartHook(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "wait" {
			pod.Spec.Containers[i].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: executorWaitStartDelaySeconds}}}
		}
	}
}
