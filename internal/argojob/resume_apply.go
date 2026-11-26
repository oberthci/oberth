package argojob

import (
	"context"
	"encoding/json"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"slices"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type resumePodsClient interface {
	List(context.Context, meta.ListOptions) (*core.PodList, error)
	Delete(context.Context, string, meta.DeleteOptions) error
}

// PrepareResume reads live objects and computes a retry before acceptance.
func (e *Executor) PrepareResume(ctx context.Context, r stateport.Run, id, commit string) (nativebinding.ResumePlan, error) {
	w, err := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		return ResumePlan{}, err
	}
	if w.Labels[nativebinding.RunLabel] != r.ID {
		if r.Origin.Resumed() {
			return ResumePlan{}, resumeRefusal("resume used; push for a full run", "predecessor never took ownership")
		}
		return ResumePlan{}, resumeRefusal("run is already resumed", "Workflow owner changed")
	}
	pods, err := e.resumePodList(ctx, w.Name)
	if err != nil {
		return ResumePlan{}, err
	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.NativeUID()})
	in := ResumeInput{Predecessor: b, Sealed: r.Admission, Workflow: w, Pods: pods, RunID: id, Commit: commit, Now: e.config.Now()}
	if r.Status == stepmodel.RunRunning {
		return e.prepareInPod(ctx, r, in)
	}
	return PlanResume(ctx, in)
}

func (e *Executor) resumePodList(ctx context.Context, name string) ([]*core.Pod, error) {
	p, ok := e.config.Pods.(resumePodsClient)
	if !ok {
		return nil, resumeRefusal(ReasonResumeStart, "live Pod reads unavailable")
	}
	list, err := p.List(ctx, meta.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + name})
	if err != nil {
		return nil, err
	}
	out := make([]*core.Pod, len(list.Items))
	for i := range list.Items {
		out[i] = &list.Items[i]
	}
	return out, nil
}

// ApplyResume only accepts the write intent retained by the Assisted insert.
// A restart can repeat deletes of absent Pods; it never creates a Workflow.
func (e *Executor) ApplyResume(ctx context.Context, id string) error {
	e.resumeMu.Lock()
	defer e.resumeMu.Unlock()
	attempt, err := e.acceptedResume(ctx, id)
	if err != nil || attempt == nil {
		return err
	}
	r, plan := attempt.run, attempt.plan
	if plan.InPod != nil {
		if !admissioncontract.AssistEnabled {
			return e.failResume(ctx, r, resumeRefusal("assist unavailable", "in-Pod resume is dormant"))
		}
		return e.applyInPod(ctx, r, plan)
	}
	w, err := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		return e.failResume(ctx, r, err)
	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.NativeUID()})
	if verifyWorkflow(b, w) == nil {
		return e.observeResume(ctx, r)
	}
	if err = e.applyResumeWrite(ctx, r, plan, w); err != nil {
		current, readErr := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
		if readErr == nil && verifyWorkflow(b, current) == nil {
			return e.observeResume(ctx, r)
		}
		return e.failResume(ctx, r, errors.Join(err, readErr))
	}
	return e.observeResume(ctx, r)
}

func (e *Executor) observeResume(ctx context.Context, r stateport.Run) error {
	if stopped, err := e.resumeStopped(ctx, r); stopped || err != nil {
		return err
	}
	return e.restoreSnapshot(ctx, r, nil)
}

// A stop may have arrived before the ownership patch's reply was received.
// Cancel only an object already owned by R2; R1 is never changed by this path.
func (e *Executor) stopOwnedResume(ctx context.Context, r stateport.Run) error {
	w, err := e.config.Workflows.Get(ctx, r.Origin.WorkflowName, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.NativeUID()})
	if verifyWorkflow(b, w) != nil {
		return nil
	}
	return e.Terminate(ctx, b)
}

func (e *Executor) applyResumeWrite(ctx context.Context, r stateport.Run, plan ResumePlan, w *wf.Workflow) error {
	if err := checkResumeWrite(r, plan, w); err != nil {
		return err
	}
	if err := e.checkResumePods(ctx, w, plan); err != nil {
		return err
	}
	if err := e.deleteResumePods(ctx, plan); err != nil {
		return err
	}
	if err := e.resumeDispatchReady(ctx, r.ID); err != nil {
		return err
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": plan.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": plan.ResourceVersion},
		{"op": "add", "path": "/status", "value": plan.Workflow.Status},
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

func validateResumePods(plan ResumePlan, pods []*core.Pod) error {
	for _, p := range pods {
		owner := meta.GetControllerOf(p)
		if owner == nil || owner.UID != plan.UID {
			return ownership(nil)
		}
		if !slices.ContainsFunc(plan.ObservedPods, func(d ResumePod) bool { return d.Name == p.Name && d.UID == p.UID }) {
			return resumeRefusal(ReasonResumeStart, "Pod set changed")
		}
	}
	for _, expected := range plan.ObservedPods {
		if !slices.ContainsFunc(pods, func(p *core.Pod) bool { return p.Name == expected.Name && p.UID == expected.UID }) && !slices.ContainsFunc(plan.Pods, func(p ResumePod) bool { return p.Name == expected.Name }) {
			return resumeRefusal(ReasonResumeStart, "carried Pod disappeared")
		}
	}
	return nil
}

func (e *Executor) resumeStopped(ctx context.Context, r stateport.Run) (bool, error) {
	obs, err := e.config.Runs.Obligations(ctx, r.ID)
	if err != nil {
		return false, err
	}
	for _, o := range obs {
		if o.Kind == "stop" {
			if err := e.stopOwnedResume(ctx, r); err != nil {
				return true, err
			}
			return true, e.config.Reporter.Finished(ctx, r.ID, Observation{Status: stepmodel.RunCancelled, Reason: "cancelled"})
		}
	}
	return false, nil
}

func (e *Executor) failResume(ctx context.Context, r stateport.Run, cause error) error {
	current, readErr := e.config.Runs.Run(ctx, r.ID)
	if readErr != nil {
		return errors.Join(cause, readErr)
	}
	if terminalChild(current) {
		return cause
	}
	err := e.config.Reporter.Finished(ctx, r.ID, Observation{Status: stepmodel.RunFailed, Reason: ReasonResumeStart, Detail: cause.Error()})
	return errors.Join(cause, err)
}

type resumeAttempt struct {
	run  stateport.Run
	plan ResumePlan
}

// ResumeFailed settles an accepted retry whose source retention failed.
func (e *Executor) ResumeFailed(ctx context.Context, id string, cause error) error {
	r, err := e.config.Runs.Run(ctx, id)
	if err != nil {
		return err
	}
	if !r.Origin.Resumed() {
		return resumeRefusal(ReasonResumeStart, "successor is not resumed")
	}
	var plan ResumePlan
	if err = json.Unmarshal(r.Origin.ResumePlan, &plan); err != nil {
		return errors.Join(cause, err)
	}
	return e.settleResumeFailure(ctx, r, plan, cause)
}

func (e *Executor) acceptedResume(ctx context.Context, id string) (*resumeAttempt, error) {
	r, err := e.config.Runs.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if !r.Origin.Resumed() || !r.Origin.Assisted {
		return nil, resumeRefusal(ReasonResumeStart, "successor is not active")
	}
	var plan ResumePlan
	if err = json.Unmarshal(r.Origin.ResumePlan, &plan); err != nil || plan.Workflow == nil {
		return nil, resumeRefusal(ReasonResumeStart, "missing accepted write")
	}
	if terminalChild(r) {
		e.releaseInPodHold(ctx, r, plan)
		return nil, resumeRefusal(ReasonResumeStart, "successor is not active")
	}
	if stopped, err := e.resumeStopped(ctx, r); stopped || err != nil {
		if stopped {
			e.releaseInPodHold(ctx, r, plan)
		}
		return nil, err
	}
	if e.config.Keeper != nil {
		if err = e.config.Keeper.Keep(ctx, r.ID, r.Admission.SourceBindings); err != nil {
			return nil, e.settleResumeFailure(ctx, r, plan, err)
		}
	}
	return &resumeAttempt{run: r, plan: plan}, nil
}

func (e *Executor) settleResumeFailure(ctx context.Context, r stateport.Run, plan ResumePlan, cause error) error {
	if plan.InPod != nil {
		return e.failInPod(ctx, r, plan, cause)
	}
	return e.failResume(ctx, r, cause)
}

func checkResumeWrite(r stateport.Run, plan ResumePlan, w *wf.Workflow) error {
	if w.DeletionTimestamp != nil || w.UID != plan.UID || w.ResourceVersion != plan.ResourceVersion || w.Labels[nativebinding.RunLabel] != r.Origin.ResumedFrom {
		return resumeRefusal(ReasonResumeStart, "Workflow changed")
	}
	return nil
}

func (e *Executor) checkResumePods(ctx context.Context, w *wf.Workflow, plan ResumePlan) error {
	pods, err := e.resumePodList(ctx, w.Name)
	if err != nil {
		return err
	}
	if err = resumeWindow(w, pods, e.config.Now()); err != nil {
		return err
	}
	if err = validateResumePods(plan, pods); err != nil {
		return err
	}
	return nil
}

func (e *Executor) deleteResumePods(ctx context.Context, plan ResumePlan) error {
	var err error
	client := e.config.Pods.(resumePodsClient)
	for _, p := range plan.Pods {
		if p.UID == "" {
			continue
		}
		if err = client.Delete(ctx, p.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &p.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (e *Executor) resumeDispatchReady(ctx context.Context, id string) error {
	current, err := e.config.Runs.Run(ctx, id)
	if err != nil {
		return err
	}
	if terminalChild(current) {
		return resumeRefusal(ReasonResumeStart, "successor ended")
	}
	if stopped, err := e.resumeStopped(ctx, current); stopped || err != nil {
		if err != nil {
			return err
		}
		return resumeRefusal(ReasonResumeStart, "successor stopped")
	}
	return nil
}
