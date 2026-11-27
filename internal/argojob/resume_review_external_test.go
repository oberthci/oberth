package argojob_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
)

func TestReviewProbeInPodPlanDynamicSecret(t *testing.T) {
	in, h, _ := inPodFixture(t)
	in.Sealed.Plan.Entries = append(in.Sealed.Plan.Entries, admissioncontract.PlanEntry{Path: "fanout", Name: "deploy", Template: "deploy", Dynamic: true, SecretRoute: true})
	plan, err := argojob.PlanInPod(t.Context(), in, h)
	t.Logf("PROBE in-pod plan: err=%v carried=%v remaining=%v", err, plan.Carried, plan.Remaining)
	if err == nil {
		t.Fatal("PROBE-FAIL: in-Pod plan admitted with an unstarted dynamic secret step remaining")
	}
}

func TestReviewProbeRetrySecretStepsDynamic(t *testing.T) {
	in := resumeFixture(t, resumeDAG)
	in.Sealed.Plan.Entries = append(in.Sealed.Plan.Entries, admissioncontract.PlanEntry{Path: "fanout", Name: "deploy", Template: "step", Dynamic: true, SecretRoute: true})
	plan, err := argojob.PlanResume(t.Context(), in)
	t.Logf("PROBE retry plan: err=%v secret=%v remaining=%v", err, plan.SecretSteps, plan.Remaining)
	if err == nil && len(plan.SecretSteps) == 0 {
		t.Fatal("PROBE-FAIL: retry plan names no secret step although a dynamic secret step remains")
	}
}
