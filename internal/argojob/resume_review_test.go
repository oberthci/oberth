package argojob

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
)

// Review probe: an unstarted dynamic secret entry must still refuse inspection.
func TestReviewProbeInspectionDynamicSecret(t *testing.T) {
	config, r, plan, native, pods := inPodRecovery(t)
	native.w = plan.Workflow.DeepCopy()
	r.Admission.Plan.Entries = append(r.Admission.Plan.Entries, admissioncontract.PlanEntry{Path: "fanout", Name: "deploy", Template: "deploy", Dynamic: true, SecretRoute: true})
	config.Runs.(*statefake.Runs).SeedRun(r)
	e := New(config)
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: "ci", Name: "shared", UID: "uid"})
	q := HoldExecRequest{Binding: b, Hold: resumeLiveHold(r, plan), Command: []string{holdBinary, "rerun", "--pod-uid", "x", "--sha256", "y"}}
	_, err := e.HoldExec(t.Context(), q)
	t.Logf("PROBE inspection with unstarted dynamic secret step: err=%v inspections=%d", err, pods.inspections)
	if err == nil {
		t.Fatal("PROBE-FAIL: inspection (here invoking the rerun helper) admitted while a dynamic secret step remains")
	}
}
