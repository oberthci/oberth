package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/store"
)

// reusablePromotionPlan retains the tested merge object only after a push-only
// infrastructure failure and a fresh read proving its planned base unchanged.
// A target that already holds the exact result needs finalization, not tests.
// Admission still rechecks source health, actor scope and promotion grants.
func (service *API) reusablePromotionPlan(ctx context.Context, repository model.Repository, promotion model.Promotion) (gitcache.MergeCandidate, bool, error) {
	lineage, err := service.promotions.PromotionLineage(ctx, promotion.ID)
	if err != nil || lineage.RetryOf == "" {
		return gitcache.MergeCandidate{}, false, err
	}
	failed, err := service.promotions.FailedPromotionPublication(ctx, lineage.RetryOf)
	if errors.Is(err, store.ErrNotFound) {
		return gitcache.MergeCandidate{}, false, nil
	}
	if err != nil {
		return gitcache.MergeCandidate{}, false, err
	}
	if !pushretry.PublicationFailure(failed.Error) && !pushretry.Transient(errors.New(failed.Error)) {
		return gitcache.MergeCandidate{}, false, nil
	}
	if failed.RepoID != repository.ID || failed.Ref != promotion.TargetRef || !validOID(failed.ResultSHA) || !failed.PreviousKnown {
		return gitcache.MergeCandidate{}, false, fmt.Errorf("retry publication does not match promotion")
	}
	if failed.RunID != "" {
		if service.history == nil {
			return gitcache.MergeCandidate{}, false, refuse(ErrUnavailable, "promotion test history")
		}
		run, err := service.history.Run(ctx, failed.RunID)
		if err != nil {
			return gitcache.MergeCandidate{}, false, err
		}
		if !reusablePublicationRun(run, failed) {
			return gitcache.MergeCandidate{}, false, nil
		}
	}
	remote, exists, err := service.git.RemoteRef(ctx, service.cacheInputFor(ctx, repository), "refs/heads/"+promotion.TargetRef)
	if err != nil {
		return gitcache.MergeCandidate{}, false, err
	}
	state := classifyRemotePublication(failed, remote, exists)
	if state == remotePublicationOther {
		return gitcache.MergeCandidate{}, false, nil
	}
	unborn := failed.PreviousSHA == "" || sameOID(failed.PreviousSHA, zeroOID)
	base := failed.PreviousSHA
	if unborn {
		base = ""
	}
	// FastForward here selects the no-new-CI delivery path; the original
	// non-fast-forward merge SHA is preserved verbatim.
	return gitcache.MergeCandidate{BaseSHA: base, MergedSHA: failed.ResultSHA, FastForward: true, TargetUnborn: unborn}, true, nil
}

func reusablePublicationRun(run model.Run, publication model.Publication) bool {
	tested := run.TestedSHA
	if tested == "" {
		tested = run.SHA
	}
	pushOnly := (run.Phase == "infrastructure" && pushretry.PublicationFailure(run.Error)) || (run.Phase == "publishing" && pushretry.Transient(errors.New(run.Error)))
	transient := pushretry.PublicationFailure(publication.Error) || pushretry.Transient(errors.New(publication.Error))
	return transient && pushOnly && run.RepoID == publication.RepoID && run.Status == model.RunFailed && run.FailedStep == "" && sameOID(run.SHA, publication.ResultSHA) && sameOID(tested, publication.ResultSHA)
}
