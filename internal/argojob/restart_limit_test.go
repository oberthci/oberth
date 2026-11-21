package argojob_test

import (
	"encoding/json"
	"os"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

type restartLimitCapture struct {
	wf.WorkflowStatus
	KnownLimit struct {
		Initial, Unobserved      wf.NodeStatus
		FirstPod, ReplacementPod core.Pod
		Attempted, Recovered     *wf.NodeStatus
		DeleteUID                string
	}
}

// Captured native loss sequences intentionally pin today's ordinary outcome.
// A native fix or a different evidence ruling must flip this limit before assist is enabled.
func TestKnownLimitUnrecordedPodReplacementRemainsOrdinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, capture string
		lostWrite     bool
	}{
		{name: "restart-deletion-survives-failed-status-write", capture: "restart-write-failed", lostWrite: true},
		{name: "missing-pending-pod-recreated", capture: "restart-pending-missed-pod", lostWrite: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/armed-evidence-" + tc.capture + ".json")
			compositionOK(t, err)
			var f restartLimitCapture
			compositionOK(t, json.Unmarshal(raw, &f))
			assertUnrecordedReplacement(t, f)
			assertCapturedReplacementPods(t, f)
			if tc.lostWrite {
				assertLostRestartWrite(t, f)
			}
			// The first execution was not observed. Only this final native result reaches finish.
			conditionalFinish(t, conditionalEvidenceCase{name: tc.name, capture: tc.capture, restarted: false, excluded: false})
		})
	}
}

func assertUnrecordedReplacement(t *testing.T, f restartLimitCapture) {
	t.Helper()
	s := f.KnownLimit
	if f.Phase != wf.WorkflowSucceeded || len(f.Nodes) != 1 {
		t.Fatal("known-limit capture lost final success", f.WorkflowStatus)
	}
	var final wf.NodeStatus
	for _, n := range f.Nodes {
		final = n
	}
	if final.Phase != wf.NodeSucceeded || final.FailedPodRestarts != 0 {
		t.Fatal("capture lost zero-counter success", final)
	}
	for _, n := range []wf.NodeStatus{s.Initial, s.Unobserved} {
		if n.ID != final.ID || n.Phase != wf.NodePending || n.FailedPodRestarts != 0 {
			t.Fatal("capture lost unobserved execution window", n)
		}
	}
}

func assertCapturedReplacementPods(t *testing.T, f restartLimitCapture) {
	t.Helper()
	s := f.KnownLimit
	a, b := s.FirstPod, s.ReplacementPod
	if a.UID == "" || b.UID == "" || a.UID == b.UID || a.Name != b.Name || b.Status.Phase != core.PodSucceeded {
		t.Fatal("capture lost same-node Pod replacement", a.ObjectMeta, b.ObjectMeta)
	}
	for _, p := range []core.Pod{a, b} {
		if p.Annotations["workflows.argoproj.io/node-id"] != s.Initial.ID {
			t.Fatal("capture lost Pod/node binding", p.ObjectMeta)
		}
	}
	for _, c := range a.Status.ContainerStatuses {
		if c.Name == "main" && c.State.Running != nil {
			return
		}
	}
	t.Fatal("capture lost unobserved running main container")
}

func assertLostRestartWrite(t *testing.T, f restartLimitCapture) {
	t.Helper()
	s := f.KnownLimit
	if s.Attempted == nil || s.Recovered == nil {
		t.Fatal("lost-write capture is incomplete")
	}
	if s.Attempted.ID != s.Initial.ID || s.Attempted.FailedPodRestarts != 1 || s.Attempted.Phase != wf.NodePending {
		t.Fatal("native restart intent missing", s.Attempted)
	}
	if s.Recovered.ID != s.Initial.ID || s.Recovered.FailedPodRestarts != 0 || s.Recovered.Phase != wf.NodePending {
		t.Fatal("capture lost durable zero-counter recovery", s.Recovered)
	}
	if s.DeleteUID != string(s.FirstPod.UID) {
		t.Fatal("capture lost UID-fenced deletion", s.DeleteUID)
	}
}
