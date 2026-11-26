package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Executor) applyInPod(ctx context.Context, r stateport.Run, plan ResumePlan) error {
	w, err := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		return e.failResume(ctx, r, err)
	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: w.Name, UID: r.Origin.NativeUID()})
	if verifyWorkflow(b, w) != nil {
		if err = e.relabelInPod(ctx, r, plan, w); err != nil {
			// A patch may have committed despite a lost reply. Re-read the
			// native owner before deciding whether to release or dispatch.
			w, readErr := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
			if readErr != nil || verifyWorkflow(b, w) != nil {
				return e.failInPod(ctx, r, plan, errors.Join(err, readErr))
			}
		}
	}
	if err = e.resumeDispatchReady(ctx, r.ID); err != nil {
		return e.failInPod(ctx, r, plan, err)
	}
	if err = e.dispatchInPod(ctx, r, plan); err != nil {
		return err
	}
	return e.observeResume(ctx, r)
}

func (e *Executor) relabelInPod(ctx context.Context, r stateport.Run, plan ResumePlan, w *wf.Workflow) error {
	if err := checkResumeWrite(r, plan, w); err != nil {
		return err
	}
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return err
	}
	pods, err := e.resumePodList(ctx, w.Name)
	if err != nil {
		return err
	}
	if err = validateResumePods(plan, pods); err != nil {
		return err
	}
	if err = inPodWindow(w, pods, *plan.InPod); err != nil {
		return err
	}
	h := resumeLiveHold(r, plan)
	status := e.queryHold(ctx, &h)
	if h.AvailabilityUnknown || status.OriginalExit != plan.InPod.OriginalExit {
		return resumeRefusal(ReasonResumeStart, "hold changed before handoff")
	}
	if err = e.resumeDispatchReady(ctx, r.ID); err != nil {
		return err
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": plan.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": plan.ResourceVersion},
		{"op": "add", "path": "/metadata/labels", "value": plan.Workflow.Labels},
		{"op": "add", "path": "/metadata/annotations", "value": plan.Workflow.Annotations},
		{"op": "add", "path": "/metadata/finalizers", "value": plan.Workflow.Finalizers},
	})
	if err != nil {
		return err
	}
	_, err = e.config.Workflows.Patch(ctx, PatchRequest{Name: w.Name, Body: body})
	return err
}

func resumeLiveHold(r stateport.Run, plan ResumePlan) model.LiveHold {
	h := plan.InPod
	return model.LiveHold{HeldNotice: model.HeldNotice{RunID: r.ID, Step: h.Step, NodeID: h.NodeID, PodUID: h.PodUID}, PodName: h.PodName}
}

func (e *Executor) dispatchInPod(ctx context.Context, r stateport.Run, plan ResumePlan) error {
	h := resumeLiveHold(r, plan)
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.NativeUID()})
	w, err := e.inPodLiveWorkflow(ctx, b)
	if err != nil {
		return err
	}
	n, exists := w.Status.Nodes[h.NodeID]
	if exists && n.Phase.Fulfilled(nil) {
		return nil
	}
	p, err := e.config.Pods.Get(ctx, h.PodName, meta.GetOptions{})
	if err != nil {
		return err
	}
	if !exists || !holdBindingMatches(w, n, p, h) {
		return e.failInPod(ctx, r, plan, resumeRefusal(ReasonResumeStart, "held Pod binding changed"))
	}
	if p.Status.Phase == core.PodSucceeded || p.Status.Phase == core.PodFailed {
		return nil
	}
	status, err := e.inPodStatus(ctx, h)
	if err != nil {
		return err
	} // Uncertain status is recoverable; never guess.
	if status.Type != "held" {
		return nil
	}
	if status.OriginalExit != plan.InPod.OriginalExit {
		return e.failInPod(ctx, r, plan, fmt.Errorf("held exit changed"))
	}
	return e.sendInPodRerun(ctx, r, plan)
}

func (e *Executor) inPodLiveWorkflow(ctx context.Context, b nativebinding.RunBinding) (*wf.Workflow, error) {
	w, err := e.config.Workflows.Get(ctx, b.Workflow.Name, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err = verifyWorkflow(b, w); err != nil {
		return nil, err
	}
	if err = packer.DecompressWorkflow(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

func (e *Executor) sendInPodRerun(ctx context.Context, r stateport.Run, plan ResumePlan) error {
	h := resumeLiveHold(r, plan)
	exporter := e.config.ResumeExport
	if exporter == nil {
		return e.failInPod(ctx, r, plan, fmt.Errorf("checkout export unavailable"))
	}
	input, err := resumeExport(ctx, exporter, *plan.InPod)
	if err != nil {
		return e.failInPod(ctx, r, plan, err)
	}
	defer input.Close()
	if err = e.resumeDispatchReady(ctx, r.ID); err != nil {
		return err
	}
	output := &boundedOutput{limit: 4096}
	err = e.config.Pods.Exec(ctx, ExecRequest{Pod: h.PodName, Container: "main", Command: []string{holdBinary, "rerun", "--pod-uid", h.PodUID, "--sha256", plan.InPod.Digest}, In: input, Out: &boundedOutput{limit: 4096}, Err: output})
	frame, _, valid := holdReply(output.body.Bytes())
	if valid && (frame.Type == "rerunning" || frame.Type == "already_rerun" || frame.Type == "closed") {
		return nil
	}
	// A lost reply can hide an accepted rerun. Its one-shot state settles it.
	status, statusErr := e.inPodStatus(ctx, h)
	if statusErr == nil && status.Type != "held" {
		return nil
	}
	if statusErr != nil {
		return errors.Join(err, statusErr)
	}
	return e.failInPod(ctx, r, plan, errors.Join(err, fmt.Errorf("snapshot dispatch refused")))
}

func (e *Executor) inPodStatus(ctx context.Context, h model.LiveHold) (holdFrame, error) {
	output := &boundedOutput{limit: 4096}
	err := e.config.Pods.Exec(ctx, ExecRequest{Pod: h.PodName, Container: "main", Command: []string{holdBinary, "status", "--pod-uid", h.PodUID}, Out: &boundedOutput{limit: 4096}, Err: output})
	frame, _, ok := holdReply(output.body.Bytes())
	if !ok || err != nil {
		return holdFrame{}, errors.Join(err, fmt.Errorf("hold status unavailable"))
	}
	return frame, nil
}

func (e *Executor) failInPod(ctx context.Context, r stateport.Run, plan ResumePlan, cause error) error {
	e.releaseInPodHold(ctx, r, plan)
	return e.failResume(ctx, r, cause)
}

func (e *Executor) releaseInPodHold(ctx context.Context, r stateport.Run, plan ResumePlan) {
	if plan.InPod == nil || r.Status == stepmodel.RunPassed {
		return
	}
	// Release only the exact original attempt, under whichever admitted owner
	// still holds the labels. Failure to release grants no execution authority.
	w, err := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
	if err == nil && w.UID == plan.UID && (w.Labels[nativebinding.RunLabel] == r.ID || w.Labels[nativebinding.RunLabel] == r.Origin.ResumedFrom) {
		h := resumeLiveHold(r, plan)
		h.RunID = w.Labels[nativebinding.RunLabel]
		p, readErr := e.config.Pods.Get(ctx, h.PodName, meta.GetOptions{})
		if readErr == nil && holdBindingMatches(w, w.Status.Nodes[h.NodeID], p, h) {
			_, _ = e.relayHold(ctx, h, []string{holdBinary, "release", "--pod-uid", h.PodUID}, 4096)
		}
	}
}
