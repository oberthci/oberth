package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// prepareReplayTagCheck resolves DB scope before entering the single writer.
// The callback rechecks the exact local tag object and returns whether it
// moved; nil means unknown, not an observed move or match.
func (service *API) prepareReplayTagCheck(ctx context.Context, run model.Run) (func(model.Run) *bool, error) {
	resolver, ok := service.refs.(interface {
		TagObjectSHA(context.Context, string, string) (string, error)
	})
	if !ok {
		return func(model.Run) *bool { return nil }, nil
	}
	repository, err := service.repositoryByID(ctx, run.RepoID)
	if err != nil {
		return nil, err
	}
	input := service.cacheInputFor(ctx, repository)
	return func(current model.Run) *bool {
		sha, err := resolver.TagObjectSHA(ctx, input, current.Ref)
		if err != nil {
			return nil
		}
		moved := !sameOID(sha, current.SHA)
		return &moved
	}, nil
}

func (service *API) releaseReplayDecision(run model.Run, evidence model.RunReplayEvidence, tagCurrent bool) (model.RunReplayAudit, bool) {
	audit := model.RunReplayAudit{Basis: "unknown"}
	if !evidence.ChainComplete || !evidence.Newest || !tagCurrent || len(evidence.Chain) == 0 {
		return audit, false
	}
	preIntent := true
	for _, attempt := range evidence.Chain {
		audit.RunsChecked++
		audit.PublicationStarted = audit.PublicationStarted || attempt.PublicationStarted
		count, safe := service.releaseAttemptSafe(attempt)
		audit.StepsChecked += count
		if !safe {
			return audit, false
		}
		preIntent = preIntent && attempt.Run.WorkflowName == ""
	}
	audit.Basis = "observed-terminal-no-credential-start"
	if preIntent {
		audit.Basis = "pre-intent"
	}
	return audit, true
}

// releaseAttemptSafe uses only stored native node/grant evidence. Marker text
// and old journals cannot grant authority to re-execute credentialed work.
func (service *API) releaseAttemptSafe(attempt model.RunReplayAttempt) (int, bool) {
	run, rows := attempt.Run, attempt.Steps
	if !run.Status.Terminal() || attempt.PublicationStarted || run.Phase == "publishing" {
		return 0, false
	}
	if run.WorkflowName == "" {
		return 0, len(rows) == 0 && run.FailedStep == ""
	}
	observation := attempt.Observation
	if observation == nil || observation.NodeIDs == nil || observation.Version != 1 || observation.WorkflowName != run.WorkflowName || observation.Nodes < len(observation.Steps) || observation.Nodes != len(rows) || len(observation.NodeIDs) != observation.Nodes {
		return len(rows), false
	}
	if observation.Nodes == 0 {
		return 0, false
	}
	// Every final Argo node must have its own stored row. This also rejects
	// a partial batch, a lost retry attempt or historical rows lacking IDs.
	expected := make(map[string]bool, len(observation.NodeIDs))
	for _, id := range observation.NodeIDs {
		if id == "" || expected[id] {
			return len(rows), false
		}
		expected[id] = true
	}
	executions := 0
	for _, step := range rows {
		if !expected[step.NodeID] {
			return len(rows), false
		}
		delete(expected, step.NodeID)
		if !step.ReplayEvidenceComplete || !step.Status.Terminal() {
			// An unfinished node on a terminal Workflow cannot prove that
			// credentialed work did not start.
			return len(rows), false
		}
		switch step.NodeType {
		case "DAG", "Steps", "StepGroup", "Retry", "TaskGroup":
			// These nodes group execution; their children carry authority.
			continue
		case "Skipped":
			executions++
			if step.Phase == "Skipped" || step.Phase == "Omitted" {
				continue
			}
		}
		if step.NodeType != "Skipped" {
			executions++
		}
		if step.Template == "" || !step.CredentialEvidenceKnown || step.Credentialed {
			return len(rows), false
		}
	}
	return len(rows), len(expected) == 0 && executions >= len(observation.Steps) && (executions > 0 || run.FailedStep == "")
}
