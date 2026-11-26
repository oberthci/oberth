package argojob

import (
	"context"
	"fmt"
	"slices"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

// PlanInPod performs no effects. Its caller must have admitted assist and read
// the helper's live status for this exact Pod and final attempt.
func PlanInPod(ctx context.Context, in ResumeInput, held nativebinding.ResumeHold) (ResumePlan, error) {
	if err := verifyWorkflow(in.Predecessor, in.Workflow); err != nil {
		return ResumePlan{}, err
	}
	w := in.Workflow.DeepCopy()
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return ResumePlan{}, err
	}
	if err := inPodWindow(w, in.Pods, held); err != nil {
		return ResumePlan{}, err
	}
	if err := sameLiveSpec(in.Sealed.Workflow, w); err != nil {
		return ResumePlan{}, err
	}
	o, err := Project(ctx, Projection{Plan: in.Sealed.Plan, Workflow: w})
	if err != nil {
		return ResumePlan{}, err
	}
	plan := ResumePlan{Workflow: w, UID: w.UID, ResourceVersion: w.ResourceVersion, InPod: &held}
	for _, p := range in.Pods {
		plan.ObservedPods = append(plan.ObservedPods, ResumePod{Name: p.Name, UID: p.UID})
	}
	plan.Carried, plan.Remaining, err = inPodSteps(in.Sealed.Plan, o.Steps, held)
	if err != nil {
		return ResumePlan{}, err
	}
	held.DeadlineRemainingMS, held.RetryAdvised = inPodDeadline(w, in.Pods, held, in.Now)
	held.RunID = in.RunID
	plan.InPod = &held
	relabel(w, in.RunID, in.Commit)
	if err = packer.CompressWorkflowIfNeeded(ctx, w); err != nil {
		return ResumePlan{}, resumeRefusal(reasonResumeTooLarge, err.Error())
	}
	return plan, nil
}

func inPodSteps(plan admissioncontract.Plan, steps []stepmodel.RunStep, held nativebinding.ResumeHold) ([]string, []string, error) {
	carried, remaining := []string{}, []string{}
	for _, s := range steps {
		if s.Status == stepmodel.StepPassed && !resumeWorkflowHook(s.Path) {
			carried = append(carried, s.Path)
			continue
		}
		remaining = append(remaining, s.Path)
		i := slices.IndexFunc(plan.Entries, func(e admissioncontract.PlanEntry) bool { return e.Path == s.Path })
		if i < 0 {
			return nil, nil, resumeRefusal(ReasonResumeUnsafe, "eligibility unresolved")
		}
		if err := inPodRemainingEntry(plan.Entries[i], held); err != nil {
			return nil, nil, err
		}
	}
	if !slices.Contains(remaining, held.Step) {
		return nil, nil, resumeRefusal(ReasonResumeStart, "held step not in remaining set")
	}
	return carried, remaining, nil
}

func inPodRemainingEntry(e admissioncontract.PlanEntry, held nativebinding.ResumeHold) error {
	if e.Dynamic {
		return resumeRefusal(ReasonResumeUnsafe, "eligibility unresolved")
	}
	if e.SecretRoute {
		return inPodSecretRefusal()
	}
	if e.Path == held.Step && (!e.HoldArmed || !e.HoldEligible || e.ResultConsumed) {
		return resumeRefusal("held result cannot resume", "outputs.result would mix failed and fixed output, or the hold is ineligible")
	}
	return nil
}

func inPodSecretRefusal() error {
	return publicerr.New(publicerr.InvalidTransition, "secret step after rerun", "Release the uninspected hold; use the fresh-Pod resume or push for a full run.", nil)
}

func inPodWindow(w *wf.Workflow, pods []*core.Pod, h nativebinding.ResumeHold) error {
	if w.Status.Phase != wf.WorkflowRunning || w.DeletionTimestamp != nil || w.Spec.Shutdown != "" || w.Status.OffloadNodeStatusVersion != "" || workspaceDeleted(w) || h.OriginalExit <= 0 || h.OriginalExit > 255 {
		return resumeRefusal(ReasonResumeStart, "Workflow or hold is not live")
	}
	p, err := onlyHeldPod(w, pods, h)
	if err != nil {
		return err
	}
	if !inPodAttemptMatches(w, p, h) {
		return resumeRefusal(ReasonResumeStart, "held attempt changed")
	}
	return nil
}

func onlyHeldPod(w *wf.Workflow, pods []*core.Pod, h nativebinding.ResumeHold) (*core.Pod, error) {
	var heldPodObject *core.Pod
	for _, p := range pods {
		if !controlledBy(w, p) {
			return nil, ownership(nil)
		}
		if p.DeletionTimestamp != nil {
			return nil, resumeRefusal(ReasonResumeStart, "Pod is being deleted")
		}
		if string(p.UID) == h.PodUID && p.Name == h.PodName {
			heldPodObject = p
			continue
		}
		if p.Status.Phase != core.PodSucceeded && p.Status.Phase != core.PodFailed {
			return nil, resumeRefusal(ReasonResumeStart, "another Pod is still running")
		}
	}
	return heldPodObject, nil
}

func inPodAttemptMatches(w *wf.Workflow, p *core.Pod, h nativebinding.ResumeHold) bool {
	n, exists := w.Status.Nodes[h.NodeID]
	return exists && p != nil && p.Status.Phase == core.PodRunning && heldPod(p) && holdArmed(w, n) && podOwned(w, n, p) && nodePath(w, n) == h.Step && n.Phase == wf.NodeRunning
}

func inPodDeadline(w *wf.Workflow, pods []*core.Pod, h nativebinding.ResumeHold, now time.Time) (int64, bool) {
	var deadline time.Time
	add := func(start time.Time, seconds *int64) {
		if seconds == nil || start.IsZero() {
			return
		}
		d := start.Add(time.Duration(*seconds) * time.Second)
		if deadline.IsZero() || d.Before(deadline) {
			deadline = d
		}
	}
	add(w.Status.StartedAt.Time, w.Spec.ActiveDeadlineSeconds)
	for _, p := range pods {
		if string(p.UID) == h.PodUID && p.Status.StartTime != nil {
			add(p.Status.StartTime.Time, p.Spec.ActiveDeadlineSeconds)
		}
	}
	if deadline.IsZero() {
		return -1, false
	}
	left := max(int64(0), deadline.Sub(now).Milliseconds())
	n := w.Status.Nodes[h.NodeID]
	duration := h.HeldSince.Sub(n.StartedAt.Time)
	return left, !n.StartedAt.IsZero() && time.Duration(left)*time.Millisecond < duration
}

func (e *Executor) prepareInPod(ctx context.Context, r stateport.Run, in ResumeInput) (ResumePlan, error) {
	if !admissioncontract.AssistEnabled || !r.Origin.AssistRequested {
		return ResumePlan{}, resumeRefusal("assist unavailable", "in-Pod resume is dormant until assist is enabled")
	}
	w := in.Workflow.DeepCopy()
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return ResumePlan{}, err
	}
	in.Workflow = w
	holds := holdCandidates(w, in.Pods, r.ID)
	if len(holds) != 1 {
		return ResumePlan{}, resumeRefusal(ReasonResumeStart, "one live hold is required")
	}
	h := holds[0]
	status := e.queryHold(ctx, &h)
	if h.AvailabilityUnknown {
		return ResumePlan{}, resumeRefusal(ReasonResumeStart, "hold status unavailable")
	}
	held := nativebinding.ResumeHold{PodName: h.PodName, PodUID: h.PodUID, NodeID: h.NodeID, Step: nodePath(w, w.Status.Nodes[h.NodeID]), OriginalExit: status.OriginalExit, HeldSince: status.HeldSince}
	plan, err := PlanInPod(ctx, in, held)
	if err != nil {
		return ResumePlan{}, err
	}
	return plan, e.inPodReceipts(ctx, r.ID, in, held)
}

func (e *Executor) inPodReceipts(ctx context.Context, id string, in ResumeInput, held nativebinding.ResumeHold) error {
	if e.config.Progress == nil {
		return resumeRefusal(ReasonResumeStart, "durable attempt receipts unavailable")
	}
	nodes, err := e.config.Progress.Nodes(ctx, id)
	if err != nil {
		return err
	}
	if err = inPodNativeReceipts(in.Workflow, nodes, held.NodeID); err != nil {
		return err
	}
	for _, p := range in.Pods {
		if string(p.UID) == held.PodUID {
			continue
		}
		if !slices.ContainsFunc(nodes, func(n stateport.Node) bool {
			return n.PodUID == string(p.UID) && (n.Status == stepmodel.StepPassed || n.Status == stepmodel.StepFailed)
		}) {
			return resumeRefusal(reasonResumeFinish, fmt.Sprintf("Pod %s has no durable final receipt", p.Name))
		}
	}
	return nil
}

func inPodNativeReceipts(w *wf.Workflow, nodes []stateport.Node, held string) error {
	for _, n := range w.Status.Nodes {
		if n.Type != wf.NodeTypePod || n.ID == held || n.Phase == wf.NodePending {
			continue
		}
		if !n.Phase.Fulfilled(nil) {
			return resumeRefusal(reasonResumeFinish, "another native attempt is still running")
		}
		if !slices.ContainsFunc(nodes, func(saved stateport.Node) bool { return saved.NodeID == n.ID && nodeReceiptEnded(saved) }) {
			return resumeRefusal(reasonResumeFinish, "a native attempt has no durable final receipt")
		}
	}
	return nil
}

func nodeReceiptEnded(n stateport.Node) bool {
	return n.Status == stepmodel.StepPassed || n.Status == stepmodel.StepFailed || n.Status == stepmodel.StepSkipped
}
