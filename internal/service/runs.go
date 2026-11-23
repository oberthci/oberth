package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// FinishedRun is a trusted final native verdict; prose stays in Detail.
type FinishedRun struct {
	RunID          string
	Status         stepmodel.RunStatus
	NativePhase    *stepmodel.NativePhase
	Reason, Detail string
	Failure        *stepmodel.FailureRecord
}

func validateFinishedRun(o FinishedRun) error {
	if o.NativePhase != nil && !o.NativePhase.Valid() {
		return bad("native_phase")
	}
	if publicerr.CheckMessage(o.Reason, 40) != nil {
		return bad("display_reason")
	}
	if o.Failure != nil && (!o.Failure.Class.Valid() || !o.Failure.Retry.Action.Valid()) {
		return bad("failure")
	}
	return nil
}

// Finished commits the verdict, terminal event and optional publication intent together.
func (s *Service) Finished(ctx context.Context, o FinishedRun) error {
	if err := validateFinishedRun(o); err != nil {
		return err
	}
	if !terminal(o.Status) {
		return bad("status")
	}
	r, err := s.finishingRun(ctx, o.RunID)
	if err != nil {
		return err
	}
	if terminal(r.Status) && (r.DisplayReason == reasonPromotionUnavailable || r.DisplayReason == reasonPromotionEvidenceInvalid) {
		return nil
	}
	intent, promotion, err := s.finishIntent(ctx, r, o)
	var diagnostic error
	if err != nil {
		if r.Origin.Trigger != model.TriggerPromotion {
			return err
		}
		diagnostic = err
		o.Status = stepmodel.RunFailed
		o.Reason = reasonPromotionEvidenceInvalid
		var read promotionReadFailure
		if errors.As(err, &read) {
			o.Reason = reasonPromotionUnavailable
		}
		o.Detail = err.Error()
		intent, promotion = nil, nil
	}
	if err := s.recordFinished(ctx, finishedRecord{r, o, intent, promotion}); err != nil {
		return err
	}
	if diagnostic != nil {
		s.c.OnError(diagnostic)
	}
	return nil
}

type finishedRecord struct {
	r         stateport.Run
	o         FinishedRun
	intent    *stateport.PublicationIntent
	promotion *stateport.PromotionUpdate
}

func (s *Service) recordFinished(ctx context.Context, f finishedRecord) error {
	r, o, intent, promotion := f.r, f.o, f.intent, f.promotion
	if err := s.cancellationReason(ctx, r, &o); err != nil {
		return err
	}
	reason := o.Reason
	if reason == "" {
		reason = runReason(o.Status)
	}
	projectCI, err := s.ciIssueEvidence(ctx, r, o)
	if err != nil {
		return err
	}
	update := stateport.RunUpdate{ProjectCI: projectCI, ID: r.ID, Revision: r.Revision, Status: o.Status, Failure: o.Failure, Publication: intent, Promotion: promotion, Reason: reason, Detail: o.Detail, NativePhase: o.NativePhase}
	action := stateport.Action{Actor: r.Origin.Actor, Events: []stateport.EventWrite{s.event(r, model.EventRunFinished, reason, fmt.Sprint(r.Revision))}}

	if promotion != nil {
		action.Events = append(action.Events, s.promotionEvent(*promotion, r.Repo))
	}
	if err := s.c.Runs.Finish(ctx, action, update); err != nil {
		return err
	}
	if intent != nil {
		s.schedule(intent.Publication.ID, nil)
	}
	return nil
}

func (s *Service) event(r stateport.Run, kind model.EventKind, reason, key string) stateport.EventWrite {
	return stateport.EventWrite{Event: model.AgentEvent{Kind: string(kind), At: s.c.Now(), Repo: r.Repo, RunID: r.ID, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Summary: reason}, Dedupe: string(kind) + ":" + r.ID + ":" + key}
}
