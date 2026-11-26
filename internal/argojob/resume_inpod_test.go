package argojob_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func inPodFixture(t *testing.T) (argojob.ResumeInput, nativebinding.ResumeHold, argojob.Config) {
	t.Helper()
	config, b, _ := preparedInlineHold(t, "review-inline-dag-script")
	w := config.Workflows.(*fake.Workflows).Object.DeepCopy()
	w.ResourceVersion = "7"
	r, err := config.Runs.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	pod := config.Pods.(*fake.Pods).Object.DeepCopy()
	h := nativebinding.ResumeHold{PodName: pod.Name, PodUID: string(pod.UID), NodeID: "held", Step: "inline", OriginalExit: 7, HeldSince: compositionClock().Add(-time.Minute)}
	return argojob.ResumeInput{Predecessor: b, Sealed: r.Admission, Workflow: w, Pods: []*core.Pod{pod}, RunID: "successor", Commit: strings.Repeat("f", 40), Now: compositionClock()}, h, config
}

func TestInPodResumeLeavesNativeStatusAndPodIntact(t *testing.T) {
	t.Parallel()
	in, h, _ := inPodFixture(t)
	before := in.Workflow.DeepCopy()
	plan, err := argojob.PlanInPod(t.Context(), in, h)
	compositionOK(t, err)
	if plan.Mode() != "in-pod" || len(plan.Pods) != 0 || !slices.Equal(plan.Remaining, []string{"inline"}) || !jsonEqual(t, before.Status, plan.Workflow.Status) || !jsonEqual(t, before, in.Workflow) {
		t.Fatal("in-Pod plan changed native execution", plan)
	}
	if plan.Workflow.Labels[nativebinding.RunLabel] != in.RunID {
		t.Fatal("routing not moved")
	}
}

func TestInPodInspectedStateNeverReachesSecretStep(t *testing.T) {
	t.Parallel()
	in, h, _ := inPodFixture(t)
	in.Sealed.Plan.Entries[0].SecretRoute = true
	_, err := argojob.PlanInPod(t.Context(), in, h)
	if err == nil || refusalReason(t, err) != "secret step after rerun" {
		t.Fatal("in-Pod state reached secret", err)
	}
	in.Sealed.Plan.Entries[0].SecretRoute = false
	_, err = argojob.PlanInPod(t.Context(), in, h)
	compositionOK(t, err)
}

func TestInPodRefusesScriptResult(t *testing.T) {
	t.Parallel()
	in, h, _ := inPodFixture(t)
	in.Sealed.Plan.Entries[0].ResultConsumed = true
	_, err := argojob.PlanInPod(t.Context(), in, h)
	if err == nil || refusalReason(t, err) != "held result cannot resume" {
		t.Fatal("mixed outputs.result admitted", err)
	}
	in.Sealed.Plan.Entries[0].ResultConsumed = false
	_, err = argojob.PlanInPod(t.Context(), in, h)
	compositionOK(t, err)
}

func TestInPodNativeDormantGate(t *testing.T) {
	t.Parallel()
	in, _, config := inPodFixture(t)
	r, err := config.Runs.Run(t.Context(), in.Predecessor.RunID)
	compositionOK(t, err)
	r.Status, r.Origin.AssistRequested = stepmodel.RunRunning, true
	r.Origin.WorkflowName, r.Origin.WorkflowUID = in.Predecessor.Workflow.Name, in.Predecessor.Workflow.UID
	config.Pods.(*fake.Pods).ExecFunc = func(_ context.Context, q argojob.ExecRequest) error {
		_, err := fmt.Fprintln(q.Err, `oberth-hold: {"t":"held","heldSince":"2026-01-01T00:00:00Z","ttl":1200,"remaining_ms":60000,"original_exit":7}`)
		return err
	}
	_, err = argojob.New(config).PrepareResume(t.Context(), r, in.RunID, in.Commit)
	if err == nil || refusalReason(t, err) != "assist unavailable" {
		t.Fatal("in-Pod resume usable while assist is off", err)
	}
}

func TestInPodDeadlineAdvisesRetry(t *testing.T) {
	t.Parallel()
	in, h, _ := inPodFixture(t)
	in.Workflow.Status.StartedAt = meta.NewTime(in.Now.Add(-10 * time.Minute))
	limit := int64(610)
	for _, w := range []*wf.Workflow{in.Workflow, in.Sealed.Workflow} {
		w.Spec.ActiveDeadlineSeconds = &limit
	}
	n := in.Workflow.Status.Nodes[h.NodeID]
	n.StartedAt = meta.NewTime(in.Now.Add(-5 * time.Minute))
	in.Workflow.Status.Nodes[h.NodeID] = n
	h.HeldSince = in.Now.Add(-time.Minute)
	plan, err := argojob.PlanInPod(t.Context(), in, h)
	compositionOK(t, err)
	if !plan.InPod.RetryAdvised || plan.InPod.DeadlineRemainingMS != 10000 {
		t.Fatal("deadline not advised", plan.InPod)
	}
}

func TestInPodRefusesOtherLivePodAndReplacedAttempt(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"another Pod", "changed UID", "not final attempt", "deletion", "foreign terminal Pod"} {
		t.Run(kind, func(t *testing.T) {
			in, h, _ := inPodFixture(t)
			switch kind {
			case "foreign terminal Pod":
				p := in.Pods[0].DeepCopy()
				p.Name += "-foreign"
				p.UID += "-foreign"
				p.Status.Phase = core.PodSucceeded
				p.OwnerReferences[0].UID = "foreign"
				in.Pods = append(in.Pods, p)
			case "another Pod":
				p := in.Pods[0].DeepCopy()
				p.Name += "-other"
				p.UID += "-other"
				in.Pods = append(in.Pods, p)
			case "changed UID":
				h.PodUID = "other"
			case "not final attempt":
				h.OriginalExit = 0
			case "deletion":
				in.Workflow.DeletionTimestamp = &meta.Time{Time: in.Now}
			}
			if _, err := argojob.PlanInPod(t.Context(), in, h); err == nil {
				t.Fatal("unsafe in-Pod plan admitted")
			}
		})
	}
}
