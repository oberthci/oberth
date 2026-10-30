package argojob

import (
	"io"
	"strconv"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestRestartCutInitFailureStaysRedWithoutLogContinuity(t *testing.T) {
	for _, code := range []int32{23, 1, 137, -1, 0} {
		for _, path := range []string{"wait", "terminal"} {
			t.Run(path+"/"+strconv.Itoa(int(code)), func(t *testing.T) {
				c, wf, binding, events := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
					p.Status.Phase = corev1.PodFailed
					p.Spec.InitContainers = []corev1.Container{{Name: "install-jq", Image: "fixture"}}
					p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "install-jq", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}
					p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
				})
				var result *Completion
				var err error
				if path == "wait" {
					value, e := c.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, events, 0, binding)
					result, err = &value, e
				} else {
					result, err = c.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, events)
				}
				if err != nil || result.Interrupted != (code == 0) {
					t.Fatal("init exit became restart cut or zero exit lost cut authority", code, result, err)
				}
				if code != 0 && (result.Infrastructure || result.FailedStep == "") {
					t.Fatal("real init failure lost red attribution", code, result)
				}
			})
		}
	}
}
