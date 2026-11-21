package app

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// start consumes a sealed admission and its durable run; Create settles uncertainty.
func (s *Submitter) start(ctx context.Context, run stateport.Run, p *admissioncontract.PreparedAdmission) error {
	if err := s.keep(ctx, p.View()); err != nil {
		return s.failSubmission(ctx, run, "keep", err)
	}
	wf, err := s.d.Native.Create(ctx, p)
	if err != nil {
		if wf.Workflow.UID != "" {
			return refusal("submission", err)
		}
		return s.failSubmission(ctx, run, "create", err)
	}
	return s.stop(ctx, run.ID, wf)
}
func (s *Submitter) stop(ctx context.Context, id string, wf nativebinding.RunBinding) error {
	obligations, err := s.d.Runs.Obligations(ctx, id)
	if err != nil {
		return err
	}
	for _, o := range obligations {
		if o.Kind == "stop" && (o.Value == "Stop" || o.Value == "Terminate") {
			if o.Value == "Terminate" {
				return s.d.Native.Terminate(ctx, wf)
			}
			return s.d.Native.Stop(ctx, wf)
		}
	}
	return nil
}
func (s *Submitter) failSubmission(ctx context.Context, run stateport.Run, code string, err error) error {
	if stopped := ctx.Err(); stopped != nil || !publicerr.Permanent(err) {
		return errors.Join(stopped, refusal("submission", err))
	}
	e := refusal(code, err)
	return errors.Join(e, s.d.Runs.FailUnbound(ctx, stateport.Action{Actor: run.Origin.Actor}, stateport.UnboundFailure{RunID: run.ID, Reason: e.Reason(), Detail: e.Detail}))
}

// Recover runs before submissions begin; it never posts Create.
func (s *Submitter) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return refusal("recovery", nil)
	}
	var failures []error
	for before := int64(0); ; {
		runs, err := s.d.Runs.UnboundRuns(ctx, before, 200)
		if err != nil || len(runs) == 0 {
			return errors.Join(append(failures, err)...)
		}
		for _, run := range runs {
			failures = append(failures, s.recoverOne(ctx, run))
			before = run.Sequence
		}
	}
}
func (s *Submitter) recoverOne(ctx context.Context, run stateport.Run) error {
	v := run.Admission
	if err := s.keep(ctx, v); err != nil {
		return err
	}
	wf, found, err := s.d.Native.Find(ctx, run)
	if err != nil {
		var foreign *nativebinding.Refusal
		if errors.As(err, &foreign) {
			return s.d.Runs.FailUnbound(ctx, stateport.Action{Actor: run.Origin.Actor}, stateport.UnboundFailure{RunID: v.RunID, Reason: messages()["foreign"].reason, Detail: v.Identity.Namespace + "/" + nativebinding.WorkflowName(v.RunID) + ": " + err.Error()})
		}
		return err
	}
	if !found {
		return s.d.Runs.FailUnbound(ctx, stateport.Action{Actor: run.Origin.Actor}, stateport.UnboundFailure{RunID: v.RunID, Reason: messages()["lost"].reason, Detail: v.Identity.Namespace + "/" + nativebinding.WorkflowName(v.RunID)})
	}
	return s.stop(ctx, run.ID, wf)
}

func (s *Submitter) keep(ctx context.Context, v admissioncontract.PreparedView) error {
	ctx, cancel := s.d.Deadline(ctx, s.d.SourceTimeout)
	defer cancel()
	return s.d.Keeper.Keep(ctx, v.RunID, v.SourceBindings)
}
