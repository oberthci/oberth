package argojob

import (
	"context"
	"errors"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Children is C10's live-parent verification and durable-proof recovery port.
// ChildBinding reports both a matching child and a live-current admission.
type Children interface {
	ChildBinding(context.Context, *wf.Workflow) (nativebinding.ChildBinding, bool, bool, error)
	VerifyChild(*wf.Workflow, nativebinding.ChildBinding) error
}

// Keeper pins a run's admitted source roots; it is idempotent.
type Keeper interface {
	Keep(context.Context, string, []sourcecontract.Binding) error
}

// ChildRuns commits child custody and the ordinary run in one acceptance.
type ChildRuns interface {
	ChildProof(context.Context, string) (nativebinding.ChildBinding, error)
	ScheduleAdmission(context.Context, int64, string, string) (admissioncontract.PreparedView, error)
	AcceptBatch(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error)
}

func (e *Executor) recordChild(ctx context.Context, w *wf.Workflow) error {
	proof, err := e.config.ChildRuns.ChildProof(ctx, w.Name)
	if err == nil {
		return e.restoreChild(ctx, w, proof)
	}
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		return err
	}
	digest := w.Annotations[nativebinding.AdmissionAnnotation]
	if len(digest) != 64 || !gitoid.Valid(digest) {
		return childError(publicerr.Forbidden, childAdmissionInvalid, nil)
	}
	proof, found, current, err := e.config.Children.ChildBinding(ctx, w)
	if err != nil || !found {
		return err
	}
	v, err := e.config.ChildRuns.ScheduleAdmission(ctx, proof.Run.RepositoryID, proof.ScheduleEntry, proof.Run.Identity)
	if err != nil {
		if code := publicerr.Classify(err).Code(); code == publicerr.NotFound || current && errors.As(err, new(stateport.ScheduleAdmissionExpiredError)) {
			return childError(publicerr.Unavailable, childAdmissionPending, err)
		}
		return err
	}
	p, err := v.ScheduledChild(w)
	if err != nil {
		return childError(publicerr.Forbidden, childNotRecorded, err)
	}
	proof.Run.Identity, proof.Run.SpecSHA256 = p.View().Digest, p.View().SpecDigest
	return e.acceptChild(ctx, p, proof)
}

func (e *Executor) restoreChild(ctx context.Context, w *wf.Workflow, proof nativebinding.ChildBinding) error {
	r, err := e.config.Runs.Run(ctx, w.Name)
	if err != nil || terminalChild(r) {
		return err
	}
	child, err := boundChild(r, proof)
	if err != nil {
		return err
	}
	if err = child.verify(e.config.Children, w); err != nil {
		return err
	}
	return e.startChild(ctx, r, child)
}

// startChild keeps a recorded live child's source, then tracks it. It runs on
// every notice, so a crash or a transient failure after acceptance is repaired
// by the next one. A permanent refusal fails the run with the reason a pushed
// run gets and terminates its Workflow: a child without its source never stays
// a live owner of nothing.
func (e *Executor) startChild(ctx context.Context, r stateport.Run, child *childObservation) error {
	kept := e.config.Keeper.Keep(ctx, r.ID, r.Admission.SourceBindings)
	if kept != nil && !publicerr.Permanent(kept) {
		return kept
	}
	if kept != nil {
		// Terminate first: until it succeeds the child stays untracked, so the
		// next notice comes back here and terminates again.
		if err := e.Terminate(ctx, runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.WorkflowUID})); err != nil {
			return err
		}
		detail := kept.Error()
		var public *publicerr.Error
		if errors.As(kept, &public) && public.Detail != "" {
			detail = public.Detail
		}
		child.refused = &Observation{Status: stepmodel.RunFailed, Phase: stepmodel.NativeError, Reason: reasonKeep, Detail: detail, Failure: &stepmodel.FailureRecord{Class: stepmodel.FailureUnknown, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}}
	}
	return e.restoreSnapshot(ctx, r, child)
}

func (e *Executor) acceptChild(ctx context.Context, p *admissioncontract.PreparedAdmission, proof nativebinding.ChildBinding) error {
	v := p.View()
	if v.Repository != proof.Run.Revision.Repository || v.Ref != proof.Run.Ref || v.CommitSHA != proof.Run.Revision.Commit || v.ScheduleEntry != proof.ScheduleEntry {
		return childError(publicerr.Forbidden, childNotRecorded, nil)
	}
	id := "schedule/" + proof.Run.Workflow.UID
	request := model.CIRequest{ReceiveID: id, Repo: v.Repository, Ref: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: &v.CommitSHA, Trigger: model.TriggerSchedule, ScheduleEntry: v.ScheduleEntry}
	request.Actor.Identity, request.Actor.AuthMethod = "oberth:scheduler", "internal"
	event := stateport.EventWrite{Dedupe: id, Event: model.AgentEvent{Kind: string(model.EventRunUpdated), At: e.config.Now(), Repo: v.Repository, RunID: v.RunID, Ref: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: &v.CommitSHA, Summary: childObserved}}
	result, err := e.config.ChildRuns.AcceptBatch(ctx, stateport.BatchAcceptance{ReceiveID: id, Repository: v.Repository, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: p, Child: &proof, Events: []stateport.EventWrite{event}}}})
	if err != nil {
		return err
	}
	r := result.Runs[0]
	if terminalChild(r) {
		return nil
	}
	child, err := boundChild(r, proof)
	if err != nil {
		return err
	}
	return e.startChild(ctx, r, child)
}

func scheduledChildRun(r stateport.Run) bool {
	return r.Origin.Trigger == model.TriggerSchedule && r.Origin.WorkflowName == r.ID
}
func terminalChild(r stateport.Run) bool {
	return r.Status == stepmodel.RunPassed || r.Status == stepmodel.RunFailed || r.Status == stepmodel.RunCancelled
}
