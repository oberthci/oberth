package argojob

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

func TestPodRefusalRequiresAbsentFailedPod(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase   wf.NodePhase
		kind    wf.NodeType
		pod     *core.Pod
		message string
	}{
		{wf.NodeRunning, wf.NodeTypePod, nil, "is forbidden"},
		{wf.NodeError, wf.NodeTypeDAG, nil, "is forbidden"},
		{wf.NodeFailed, wf.NodeTypePod, &core.Pod{}, "is forbidden"},
		{wf.NodeFailed, wf.NodeTypePod, nil, "command failed"},
	} {
		got := diagnose(wf.NodeStatus{Type: tc.kind, Phase: tc.phase, Message: tc.message}, tc.pod, false, nil)
		if got.reason == reasonPodRefused {
			t.Fatal("non-admission failure classified as Pod refusal", tc)
		}
	}
}
