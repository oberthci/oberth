package argojob

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestInPodHandoffConfirmsCachedPredecessorLive(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		for _, snapshot := range []string{"predecessor", "current", "foreign-live"} {
			name := snapshot + "/apply"
			if restart {
				name = snapshot + "/restart"
			}
			t.Run(name, func(t *testing.T) { checkInPodObservation(t, restart, snapshot) })
		}
	}
}

func checkInPodObservation(t *testing.T, restart bool, snapshot string) {
	t.Helper()
	config, r, plan, native, pods := inPodRecovery(t)
	config.Cache = inPodStaleCache{native.w.DeepCopy()}
	e := New(config)
	t.Cleanup(e.queue.ShutDown)
	t.Cleanup(e.collector.queue.ShutDown)
	if err := e.applyInPod(t.Context(), r, plan); err != nil {
		t.Fatal(err)
	}
	if restart {
		e = New(config)
		t.Cleanup(e.queue.ShutDown)
		t.Cleanup(e.collector.queue.ShutDown)
		if err := e.applyInPod(t.Context(), r, plan); err != nil {
			t.Fatal(err)
		}
	}
	native.w.Status = wf.WorkflowStatus{Phase: wf.WorkflowSucceeded}
	want, reads := stepmodel.RunPassed, 1
	if snapshot == "current" {
		e.config.Cache = inPodStaleCache{native.w.DeepCopy()}
		reads = 0
	}
	if snapshot == "foreign-live" {
		native.w.Labels[nativebinding.RunLabel] = "foreign"
		want = stepmodel.RunFailed
	}
	e.config.Progress = &reviewR2Progress{}
	before := native.reads
	tr := e.tracked[native.w.Name]
	if err := e.observeRun(t.Context(), tr); err != nil {
		t.Fatal(err)
	}
	if tr.final == nil || tr.final.Status != want || native.reads-before != reads || pods.runs != 1 || native.patches != 1 {
		t.Fatal("in-Pod ownership observation differs", tr.final, native.reads-before, pods.runs, native.patches)
	}
}
