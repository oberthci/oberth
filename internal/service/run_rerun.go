package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// RunRerunStore queues a rerun of a terminal run and records its run.rerun
// audit action in one transaction (#1098).
type RunRerunStore interface {
	RerunRun(context.Context, model.RunRerunRequest) (model.RunRerunResult, error)
	RunRerunOf(context.Context, string) (string, error)
}

// Rerun modes reported by run_rerun.
const (
	rerunModeRun     = "rerun"         // a new run was queued at the same SHA and ref
	rerunModePublish = "publish_retry" // only the publication failed; it was re-pushed
)

// releaseRerunWarning is recorded with forced tag/release reruns.
const releaseRerunWarning = "release rerun re-executes release.yaml at the same tag: anything the failed run's Workflow already published stays published and must tolerate being published again; Oberth itself publishes the tag object at most once"

// authorizeRunRerun keeps actor/trigger authorization independent of replay
// evidence. Safe tag/release reruns still require an admin.
func authorizeRunRerun(run model.Run, admin bool) error {
	switch run.Trigger {
	case "promotion":
		return fmt.Errorf("%w: run %s is promotion CI; retry its promotion with promote_retry", ErrInvalidInput, run.ID)
	case "plan", "apply":
		return fmt.Errorf("%w: %s runs are re-driven by the trusted plan flow, not rerun", ErrInvalidInput, run.Trigger)
	}
	if !admin && (run.Release || run.RefKind != model.RefBranch || run.Trigger != "branch") {
		return refuse(ErrForbidden, "a non-admin uplink may rerun only branch-push runs, not %s runs", run.Trigger)
	}
	return nil
}

// RunRerunPolicy requires confirmation if a tag/release may have already
// performed credentialed or publication work. Unknown evidence is not safe.
func RunRerunPolicy(run model.Run, admin, force, safe bool) (string, error) {
	if err := authorizeRunRerun(run, admin); err != nil {
		return "", err
	}
	if run.Release || run.RefKind == model.RefTag {
		if !force && !safe {
			return "", fmt.Errorf("%w: run %s is a %s run with started or unknown credentialed/publication work; repeat with force to re-execute release.yaml (anything already published stays published)", ErrInvalidInput, run.ID, run.Trigger)
		}
		if force {
			return releaseRerunWarning, nil
		}
	}
	return "", nil
}

// rerunRun is MCP run_rerun: rerun a failed or interrupted run at the same
// SHA and ref (#1098), for the cases run_retry refuses: a failed step (a
// flaky test), a run that is not the newest of its ref, and tag/release runs.
// A run whose steps all passed and whose publication alone failed is not
// re-executed: its publication is retried exactly as publish_retry does.
func (service *API) rerunRun(ctx context.Context, actor api.Actor, id, reason string, force bool) (api.RunRerunResponse, error) {
	if strings.TrimSpace(id) == "" {
		return api.RunRerunResponse{}, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	if service.history == nil || service.runReruns == nil {
		return api.RunRerunResponse{}, refuse(ErrUnavailable, "run rerun")
	}
	reason = strings.TrimSpace(reason)
	if err := store.ValidateCancelReason(reason); err != nil {
		return api.RunRerunResponse{}, fmt.Errorf("%w: %s", ErrInvalidInput,
			strings.Replace(strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "), "cancel reason", "rerun reason", 1))
	}
	source, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return api.RunRerunResponse{}, err
	}
	switch {
	case source.Status.Active():
		return api.RunRerunResponse{}, fmt.Errorf("%w: run %s is still %s; wait for it or cancel it first", ErrInvalidInput, source.ID, source.Status)
	case source.Status == model.RunPassed:
		return api.RunRerunResponse{}, fmt.Errorf("%w: run %s already passed", ErrInvalidInput, source.ID)
	}
	if err := authorizeRunRerun(source, actor.Admin); err != nil {
		return api.RunRerunResponse{}, err
	}
	if service.enqueues == nil {
		return api.RunRerunResponse{}, refuse(ErrUnavailable, "run rerun queue")
	}
	if source.Status == model.RunFailed && source.Phase == "publishing" {
		if _, err := RunRerunPolicy(source, actor.Admin, force, false); err != nil {
			return api.RunRerunResponse{}, err
		}
		// Every step passed; re-running them would repeat a release's
		// publishing work. Re-push the exact recorded object instead.
		retried, err := service.publishRetry(ctx, actor, source.ID)
		if err != nil {
			return api.RunRerunResponse{}, err
		}
		return api.RunRerunResponse{
			Mode: rerunModePublish, RerunOf: source.ID, PublicationID: retried.PublicationID,
			Status: retried.Status, Error: retried.Error,
		}, nil
	}
	if source.RefKind == model.RefBranch {
		if err := service.requireBranchHead(ctx, source); err != nil {
			return api.RunRerunResponse{}, err
		}
	}
	audit := &model.RunReplayAudit{Basis: "branch"}
	tagCheck := func(model.Run) bool { return false }
	if !force && (source.Release || source.RefKind == model.RefTag) {
		tagCheck, err = service.prepareReplayTagCheck(ctx, source)
		if err != nil {
			return api.RunRerunResponse{}, err
		}
	}
	result, err := service.runReruns.RerunRun(ctx, model.RunRerunRequest{
		RunID: source.ID, Actor: actor.Identity, Reason: reason, Force: force, ReplayAudit: audit,
		Authorize: func(current model.Run, evidence model.RunReplayEvidence) (string, error) {
			safe := false
			if !force && (current.Release || current.RefKind == model.RefTag) {
				*audit, safe = service.releaseReplayDecision(current, evidence, tagCheck(current))
			}
			if force && (current.Release || current.RefKind == model.RefTag) {
				audit.Basis = "forced"
				audit.PublicationStarted = evidence.PublicationStarted
			}
			return RunRerunPolicy(current, actor.Admin, force, safe)
		},
	})
	if err != nil {
		return api.RunRerunResponse{}, err
	}
	enqueued := result.Enqueued
	service.signals.NotifyRun(enqueued.ID)
	response := api.RunRerunResponse{
		Mode: rerunModeRun, RunID: enqueued.ID, RerunOf: source.ID, Status: string(enqueued.Status),
		Ref: enqueued.Ref, SHA: enqueued.SHA, Warning: result.Warning,
	}
	if err := service.enqueues.AcceptEnqueue(ctx, enqueued); err != nil {
		return response, err
	}
	return response, nil
}

// requireBranchHead refuses a branch rerun once the branch has moved: a green
// branch run force-syncs its SHA upstream, so rerunning an older commit would
// roll the upstream branch back over newer pushes.
func (service *API) requireBranchHead(ctx context.Context, run model.Run) error {
	if service.refs == nil {
		return refuse(ErrUnavailable, "branch head lookup for run rerun")
	}
	repository, err := service.repositoryByID(ctx, run.RepoID)
	if err != nil {
		return err
	}
	head, err := service.refs.RefSHA(ctx, service.cacheInputFor(ctx, repository), run.Ref)
	if err != nil {
		return fmt.Errorf("%w: branch %s no longer exists in Oberth; push it again instead of rerunning run %s", ErrInvalidInput, run.Ref, run.ID)
	}
	if !sameOID(head, run.SHA) {
		return fmt.Errorf("%w: branch %s moved to %s; rerunning run %s would publish the older %s over it. Rerun the head's run or push again",
			ErrInvalidInput, run.Ref, strings.ToLower(head), run.ID, run.SHA)
	}
	return nil
}

// runRerunOf names the run a rerun re-ran, for run views; lineage lookups
// never fail the view.
func (service *API) runRerunOf(ctx context.Context, runID string) string {
	if service.runReruns == nil {
		return ""
	}
	source, err := service.runReruns.RunRerunOf(ctx, runID)
	if err != nil {
		return ""
	}
	return source
}
