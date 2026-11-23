package argojob_test

import (
	"encoding/json"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/file"
)

func TestCancelUsesLiveNodeProgress(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.NodePhase{wf.NodePending, wf.NodeSkipped, wf.NodeRunning, wf.NodeFailed, wf.NodeSucceeded} {
		for _, compressed := range []bool{false, true} {
			e, native, binding := submitted(t)
			w := native.Object
			w.Status.Phase = wf.WorkflowRunning
			w.Status.Nodes = wf.Nodes{"root": {Type: wf.NodeTypeDAG, Phase: wf.NodeRunning}, "work": {Type: wf.NodeTypePod, Phase: phase}}
			if compressed {
				raw, err := json.Marshal(w.Status.Nodes)
				compositionOK(t, err)
				w.Status.Nodes, w.Status.CompressedNodes = nil, file.CompressEncodeString(t.Context(), string(raw))
			}
			compositionOK(t, e.Stop(t.Context(), binding))
			want := wf.ShutdownStrategyStop
			if phase == wf.NodePending || phase == wf.NodeSkipped {
				want = wf.ShutdownStrategyTerminate
			}
			if native.Object.Spec.Shutdown != want {
				t.Fatal("cancel ignored live execution progress", phase, compressed, native.Object.Spec.Shutdown, want)
			}
			compositionOK(t, e.Terminate(t.Context(), binding))
			compositionOK(t, e.Stop(t.Context(), binding))
			if native.Object.Spec.Shutdown != wf.ShutdownStrategyTerminate {
				t.Fatal("repeat cancel lost escalation")
			}
		}
	}
}

func TestCancelPreservesExitHandlersWhenNativeNodesAreOffloaded(t *testing.T) {
	t.Parallel()
	e, native, binding := submitted(t)
	native.Object.Status.OffloadNodeStatusVersion = "elsewhere"
	compositionOK(t, e.Stop(t.Context(), binding))
	if native.Object.Spec.Shutdown != wf.ShutdownStrategyStop {
		t.Fatal("unread native progress was treated as unstarted")
	}
}
