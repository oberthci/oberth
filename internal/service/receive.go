package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// Decide applies receive policy before wiring prepares in the quarantine-bound gate.
// The update supplies exact object/peeled commit identities from source delivery.
func (s *Service) Decide(ctx context.Context, e RefUpdate) (Decision, error) {
	a, err := s.allow(ctx, e.Actor, "push")
	if err != nil {
		return Decision{}, err
	}
	repo, err := s.resolveRepo(ctx, a, e.Repo)
	if err != nil {
		return Decision{}, err
	}
	if repo.Archived {
		return Decision{}, archivedRepository(repo.Canonical)
	}
	policy, err := s.c.Authority.KeyPolicy(ctx, a)
	if err != nil {
		return Decision{}, err
	}
	if policy.Agent {
		defaultRef, err := s.c.Source.DefaultBranch(ctx, repo.Canonical)
		if err != nil {
			return Decision{}, err
		}
		if e.Ref == defaultRef {
			return Decision{}, refuse(promoteRequired)
		}
	}
	d := Decision{Request: model.CIRequest{ReceiveID: e.ID, Repo: repo.Canonical, Ref: e.Ref, PreviousSHA: e.PreviousSHA, ObjectSHA: e.ObjectSHA, Actor: a, Trigger: model.TriggerBranch}, Action: "run", Message: reasonQueued}
	if e.ID == "" {
		return d, bad("receive_id")
	}
	if e.ObjectSHA == "" {
		d.Action = "deleted"
		d.Message = reasonDeleted
		d.Deletion = &stateport.BranchDeletion{Request: d.Request, Publication: model.Publication{ID: "deletion:" + e.ID + ":" + e.Ref, Repo: repo.Canonical, Ref: e.Ref, Actor: a.Identity, PreviousSHA: new(e.PreviousSHA)}}
		return d, nil
	}
	return s.runDecision(ctx, receiveDecision{repo, e, policy, d})
}

type receiveDecision struct {
	repo stateport.Repository
	e    RefUpdate
	p    KeyPolicy
	d    Decision
}

func (s *Service) runDecision(ctx context.Context, input receiveDecision) (Decision, error) {
	repo, e, p, d := input.repo, input.e, input.p, input.d
	ref := gitport.Ref{Name: e.Ref, ObjectSHA: e.ObjectSHA, CommitSHA: e.CommitSHA}
	if ref.Validate() != nil || e.CommitSHA == "" {
		return d, bad("ref/object_sha/commit_sha")
	}
	d.Request.CommitSHA = &e.CommitSHA
	if strings.HasPrefix(e.Ref, "refs/tags/") {
		d.Request.Trigger = model.TriggerTag
		if IsReleaseTag(e.Ref) {
			head, err := s.c.Git.ReleaseTag(ctx, repo, ref)
			if err != nil {
				return d, err
			}
			d.Release, d.ReleaseHead = true, head
		}
	} else if p.Agent && !e.Run {
		d.Action = "parked"
		d.Message = reasonParked
	}
	return d, nil
}

// IsReleaseTag classifies stable release refs for receive, inspection and reruns.
func IsReleaseTag(ref string) bool {
	version, ok := strings.CutPrefix(ref, "refs/tags/")
	return ok && workflowpolicy.IsReleaseVersion(version)
}

// Accept records the authorized, sealed batch and its observations in one store fact.
// Structural admission/receipt validation belongs to storage; policy is decided before sealing.
func (s *Service) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	if s.ctx.Err() != nil {
		return stateport.AcceptanceResult{}, refuse(unavailable)
	}
	b.Runs = slices.Clone(b.Runs)
	b.Events = slices.Clone(b.Events)
	for i, a := range b.Runs {
		b.Runs[i] = s.acceptance(a)
	}
	result, err := s.c.Runs.AcceptBatch(ctx, b)
	if err == nil && result.Fresh {
		for _, d := range b.Deletions {
			s.schedule(d.Publication.ID, nil)
		}
	}
	return result, err
}

func (s *Service) acceptance(a stateport.Acceptance) stateport.Acceptance {
	id, message, kind := "", reasonQueued, model.EventRunUpdated
	if a.Prepared != nil {
		id = a.Prepared.View().RunID
	} else if a.Rejected != nil {
		id, message, kind = a.Rejected.ID, stateport.ReasonAdmission, model.EventRunFinished
	}
	details, _ := json.Marshal(map[string]any{
		"receive_id": a.Request.ReceiveID, "repo": a.Request.Repo, "ref": a.Request.Ref,
		"previous_sha": a.Request.PreviousSHA, "object_sha": a.Request.ObjectSHA,
		"commit_sha": a.Request.CommitSHA, "trigger": a.Request.Trigger,
		"assist": a.Request.Assist, "schedule_entry": a.Request.ScheduleEntry, "rerun_of": a.RerunOf,
	})
	a.Actions = append(slices.Clone(a.Actions), model.AuditAction{Actor: a.Request.Actor.Identity, Action: "run.request", ResourceType: "run", ResourceID: id, Details: string(details)})
	r := stateport.Run{RunSummary: stepmodel.RunSummary{ID: id, Repo: a.Request.Repo, Ref: a.Request.Ref, ObjectSHA: a.Request.ObjectSHA, CommitSHA: a.Request.CommitSHA}}
	a.Events = append(slices.Clone(a.Events), s.event(r, kind, message, "accept"))
	return a
}
