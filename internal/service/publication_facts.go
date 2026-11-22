package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Service) settle(ctx context.Context, p model.Publication, failure error) error {
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	result := stateport.PublicationResult{ID: p.ID, Attempt: p.Attempts, Delivered: failure == nil}
	action := stateport.Action{Actor: p.Actor}
	if failure != nil {
		s.failedAttempt(&result, &action, p, failure)
	} else {
		if err := s.delivered(write, p, &result); err != nil {
			return err
		}
		if result.Release != nil {
			e := s.publicationEvent(p, model.EventReleasePublished, reasonReleased)
			e.Event.Data, _ = json.Marshal(result.Release)
			action.Events = append(action.Events, e)
		}
	}
	if err := s.c.Publications.SettlePublication(write, action, result); err != nil {
		return err
	}
	current, err := s.c.Publications.Publication(write, p.ID)
	if err == nil && current.Status == model.PublicationPending {
		s.schedule(current.ID, current.RetryAt)
	}
	return err
}

// failedAttempt records a failed attempt: retried within the push-retry
// budget, then failed. A cache record held by a pending receive says so, as
// the forge may already have the commit.
func (s *Service) failedAttempt(result *stateport.PublicationResult, action *stateport.Action, p model.Publication, failure error) {
	result.Retryable = transient(failure)
	result.Reason = reasonPubFailed
	result.Detail = failure.Error()
	final := !result.Retryable || p.Attempts >= pushretry.MaxAttempts
	if !final {
		result.Reason = reasonPubRetry
	}
	var refusal *publicerr.Error
	if errors.As(failure, &refusal) && refusal.ID == gitport.ReceivePendingID {
		result.Reason = reasonPubReceive
	}
	if final {
		p.Status, p.Reason, p.Detail = model.PublicationFailed, result.Reason, result.Detail
		p.RetryAt = nil
		action.Events = []stateport.EventWrite{s.publicationEvent(p, model.EventPublicationFailed, reasonPubFailed)}
	}
}

// delivered completes a delivery's result from its run, read once. A
// promotion's publication targets the default branch; a fast-forward one
// carries the tested branch run, whose pass is then the default branch's
// newest result. A merged one carries its promotion run, which moved CI
// issues when it finished.
func (s *Service) delivered(ctx context.Context, p model.Publication, result *stateport.PublicationResult) error {
	result.Reason = reasonPubDelivered
	if p.RunID == "" {
		return nil
	}
	r, err := s.c.Runs.Run(ctx, p.RunID)
	if err != nil {
		return err
	}
	result.ProjectCI = p.PromotionID != "" && r.Origin.Trigger == model.TriggerBranch && r.Status == stepmodel.RunPassed && !r.Origin.HasAssistance()
	result.Release, err = s.releaseReceipt(ctx, p, r)
	return err
}

func (s *Service) releaseReceipt(ctx context.Context, p model.Publication, r stateport.Run) (*model.ReleaseReceipt, error) {
	if !r.Release {
		return nil, nil
	}
	if r.CommitSHA == nil || r.Origin.HasAssistance() {
		return nil, refuse(publicationEvidence)
	}
	if old, err := s.c.Publications.PublishedRelease(ctx, p.Repo, p.Ref); err == nil {
		if old.ObjectSHA != p.ObjectSHA || old.CommitSHA == nil || *old.CommitSHA != *r.CommitSHA {
			return nil, refuse(publicationEvidence)
		}
		return nil, nil
	} else if !isMissing(err) {
		return nil, err
	}
	return &model.ReleaseReceipt{Tag: r.Ref, CommitSHA: *r.CommitSHA, RunID: r.ID, PublicationID: p.ID}, nil
}
func (s *Service) publicationEvent(p model.Publication, kind model.EventKind, reason string) stateport.EventWrite {
	event := model.AgentEvent{Kind: string(kind), At: s.c.Now(), Repo: p.Repo, RunID: p.RunID, PromotionID: p.PromotionID, Ref: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: p.CommitSHA, Summary: reason}
	if p.ObjectSHA == "" {
		observation := p.Clone()
		observation.Detail = ""
		event.Data, _ = json.Marshal(observation)
	}
	return stateport.EventWrite{Event: event, Dedupe: string(kind) + ":" + p.ID}
}

// RecoverPublications rearms only durable ref intents, then gives a retest to
// a promotion whose publication a stop left abandoned as moved; wiring owns
// run recovery.
func (s *Service) RecoverPublications(ctx context.Context) error {
	q := stateport.RecordFilter{Limit: stateport.RunPageLimit}
	for {
		rows, err := s.c.Publications.PendingPublications(ctx, q)
		if err != nil {
			return err
		}
		for _, p := range rows {
			if p.Attempts > 0 && p.RetryAt == nil {
				if err := s.recoverPublication(ctx, p); err != nil {
					return err
				}
			} else {
				s.schedule(p.ID, p.RetryAt)
			}
		}
		if len(rows) < q.Limit {
			return s.recoverMovedPromotions(ctx)
		}
		q.Before = rows[len(rows)-1].ID
	}
}
func (s *Service) recoverPublication(ctx context.Context, p model.Publication) error {
	release, err := s.acquire(ctx, refKey(p.Repo, p.Ref))
	if err != nil {
		return err
	}
	defer release()
	p, err = s.c.Publications.Publication(ctx, p.ID)
	if err != nil || p.Status != model.PublicationPending {
		return err
	}
	if p.RetryAt != nil {
		s.schedule(p.ID, p.RetryAt)
		return nil
	}
	remote, err := s.remoteRef(ctx, p.Repo, p.Ref)
	if err == nil && remote.ObjectSHA == p.ObjectSHA {
		if p.PreviousSHA == nil {
			_, err = s.pushAttempt(ctx, p)
			return err
		}
		// The forge may have accepted the push before its cache transaction finished.
		receipt, err := s.c.MovingGit.Publish(ctx, p)
		if err == nil && !confirms(receipt, p) {
			err = refuse(publicationReceipt)
		}
		return s.settle(ctx, p, err)
	}
	return s.settle(ctx, p, refuse(publicationInterrupted))
}
