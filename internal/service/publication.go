package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/store"
)

type remotePublicationState uint8

const (
	remotePublicationOther remotePublicationState = iota
	remotePublicationPrevious
	remotePublicationResult
)

// deliverPublication classifies an exact durable intent against the remote
// before deciding whether a mutation is safe. Transient failures retain the
// durable intent with a bounded retry budget; every lost reply is re-read.
const publicationRetryPrefix = "upstream publication retry "

func deliverPublication(
	ctx context.Context,
	publications PublicationStore,
	git PublicationGit,
	auditor Auditor,
	repository model.Repository,
	repositoryInput string,
	publication model.Publication,
) (model.PublicationFinalization, error) {
	if publication.Status != model.PublicationPending || publication.RepoID != repository.ID {
		return model.PublicationFinalization{}, errors.New("service: publication does not match repository or is already terminal")
	}
	// repositoryInput is the fully-qualified cache input composed by the
	// caller; the bare repository.Name is ambiguous once the same name
	// exists under two upstreams (issue #264).
	if strings.TrimSpace(repositoryInput) == "" {
		repositoryInput = repository.Name
	}
	ref, err := canonicalPublicationRef(publication.RefKind, publication.Ref)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	if time.Now().Before(publication.RetryAfter) && strings.HasPrefix(publication.Error, publicationRetryPrefix) {
		return model.PublicationFinalization{Publication: publication}, nil
	}
	resume := suspendDispatch(ctx)
	remoteSHA, exists, err := git.RemoteRef(ctx, repositoryInput, ref)
	resume()
	if err != nil {
		if !publication.RetryAfter.IsZero() && time.Now().Before(publication.RetryAfter) {
			return model.PublicationFinalization{Publication: publication}, nil
		}
		failure := fmt.Errorf("read remote ref for publication %s: %w", publication.ID, err)
		readFinalizeCtx, cancelReadFinalize := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelReadFinalize()
		ctx = readFinalizeCtx
		if pushretry.Transient(failure) && publication.Attempts < pushretry.MaxAttempts {
			attempt := publication.Attempts + 1
			delay := pushretry.Delay(attempt)
			if delay == 0 {
				delay = time.Minute
			}
			publication, err = publications.UpdatePublicationAttempt(ctx, publication.ID, publication.Attempts, attempt, time.Now().Add(delay), failure.Error())
			if err != nil {
				return model.PublicationFinalization{}, err
			}
		}
		return retryPublicationFailure(ctx, publications, publication, failure)
	}
	// Read-back is safe even during backoff: a lost finalization reply must
	// never defer confirmation of a result already visible at the forge.
	if exists && sameOID(remoteSHA, publication.ResultSHA) {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationDelivered, "")
	}
	if !publication.RetryAfter.IsZero() && time.Now().Before(publication.RetryAfter) {
		return model.PublicationFinalization{Publication: publication}, nil
	}
	// Feature branches (non-promotion, non-default) use forced push with no
	// predecessor check. The default branch is excluded: a direct push to the
	// default branch must go through predecessor tracking and use a non-forced
	// push so it cannot silently overwrite a just-delivered promotion (#432).
	if publication.PromotionID == "" && publication.RefKind == model.RefBranch {
		if deleted, err := mirrorBranchDeleted(ctx, git, repositoryInput, publication.Ref); err != nil {
			return model.PublicationFinalization{}, err
		} else if deleted {
			return abandonDeletedBranchPublication(ctx, publications, publication)
		}
		matches, err := git.MirrorBranchMatches(ctx, repositoryInput, publication.Ref, publication.ResultSHA)
		if err != nil {
			return model.PublicationFinalization{}, err
		}
		if !matches {
			return abandonChangedBranchPublication(ctx, publications, publication)
		}
	}
	forceBranch := publication.PromotionID == "" && publication.RefKind == model.RefBranch && publication.Ref != repository.DefaultBranch
	if forceBranch {
		if deleted, err := mirrorBranchDeleted(ctx, git, repositoryInput, publication.Ref); err != nil {
			return model.PublicationFinalization{}, err
		} else if deleted {
			return abandonDeletedBranchPublication(ctx, publications, publication)
		}
		return attemptPublicationMutation(ctx, publications, git, auditor, repository, repositoryInput, publication, ref, true)
	}
	if !publication.PreviousKnown {
		publication, err = publications.SetPublicationPredecessor(ctx, publication.ID, remoteSHA, exists)
		if err != nil {
			return model.PublicationFinalization{}, err
		}
	}
	switch classifyRemotePublication(publication, remoteSHA, exists) {
	case remotePublicationResult:
		return finalizePublication(ctx, publications, publication.ID, model.PublicationDelivered, "")
	case remotePublicationOther:
		return finalizeMovedPublication(ctx, publications, publication, remoteSHA, exists,
			publicationConcurrencyError(publication, remoteSHA, exists).Error())
	}
	return attemptPublicationMutation(ctx, publications, git, auditor, repository, repositoryInput, publication, ref, false)
}

func attemptPublicationMutation(
	ctx context.Context,
	publications PublicationStore,
	git PublicationGit,
	auditor Auditor,
	repository model.Repository,
	repositoryInput string,
	publication model.Publication,
	ref string,
	forceBranch bool,
) (model.PublicationFinalization, error) {
	if publication.Attempts >= pushretry.MaxAttempts {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationFailed, pushretry.Failure("retry budget exhausted; "+publication.Error))
	}
	attempt := publication.Attempts + 1
	delay := pushretry.Delay(attempt)
	if delay == 0 {
		delay = time.Minute
	}
	next := time.Now().Add(delay)
	var err error
	publicationID := publication.ID
	publication, err = publications.UpdatePublicationAttempt(ctx, publication.ID, publication.Attempts, attempt, next, fmt.Sprintf("upstream publication attempt %d/%d", attempt, pushretry.MaxAttempts))
	if errors.Is(err, store.ErrPromotionGrantInactive) {
		return finalizePublication(ctx, publications, publicationID, model.PublicationFailed, err.Error())
	}
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	mutationErr := auditedGitMutation(ctx, auditor, publication.Actor,
		publicationAuditAction(publication), publicationAuditResource(publication), publicationAuditResourceID(publication),
		publicationAuditDetails(repository, publication),
		func() error { return mutatePublication(ctx, git, repositoryInput, publication, forceBranch) })
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ctx = finalizeCtx
	if errors.Is(mutationErr, gitcache.ErrMirrorBranchAdvanced) {
		return abandonChangedBranchPublication(ctx, publications, publication)
	}
	if mutationErr == nil && publication.PromotionID == "" {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationDelivered, "")
	}

	// Confirm every promotion through the configured server-side transport,
	// including successful pushes. A push acknowledgement alone is not forge
	// observation. Failed pushes also need read-back because replies can be lost.
	resume := suspendDispatch(ctx)
	remoteSHA, exists, verifyErr := git.RemoteRef(ctx, repositoryInput, ref)
	resume()
	if verifyErr != nil {
		failure := fmt.Errorf("verify remote ref after publication: %w", verifyErr)
		if mutationErr != nil {
			failure = errors.Join(fmt.Errorf("publication %s mutation was ambiguous: %w", publication.ID, mutationErr), failure)
		}
		return retryPublicationFailure(ctx, publications, publication, failure)
	}
	if mutationErr == nil {
		mutationErr = fmt.Errorf("upstream promotion target %s was not confirmed at %s", ref, publication.ResultSHA)
	}
	switch classifyRemotePublication(publication, remoteSHA, exists) {
	case remotePublicationResult:
		return finalizePublication(ctx, publications, publication.ID, model.PublicationDelivered, "")
	case remotePublicationPrevious:
		return retryPublicationFailure(ctx, publications, publication, mutationErr)
	default:
		if forceBranch {
			return retryPublicationFailure(ctx, publications, publication, mutationErr)
		}
		failure := errors.Join(mutationErr, publicationConcurrencyError(publication, remoteSHA, exists))
		return finalizeMovedPublication(ctx, publications, publication, remoteSHA, exists, failure.Error())
	}
}

// finalizeMovedPublication fails a publication whose remote ref held neither
// its planned base nor its result. For a promotion whose target still exists,
// the finalization carries the head it moved to, so the moved-target retry
// (#1097) is decided on this typed signal and never on error text.
func finalizeMovedPublication(ctx context.Context, publications PublicationStore, publication model.Publication,
	remoteSHA string, exists bool, failure string) (model.PublicationFinalization, error) {
	finalization, err := finalizePublication(ctx, publications, publication.ID, model.PublicationFailed, failure)
	if err == nil && exists && publication.PromotionID != "" && validOID(remoteSHA) {
		finalization.TargetMovedTo = strings.ToLower(remoteSHA)
	}
	return finalization, err
}

func mutatePublication(ctx context.Context, git PublicationGit, repository string, publication model.Publication, forceBranch bool) error {
	resume := suspendDispatch(ctx)
	defer resume()
	if publication.PromotionID != "" {
		return git.PushPromotion(ctx, repository, publication.Ref, publication.ResultSHA)
	}
	if publication.RefKind == model.RefTag {
		return git.SyncTag(ctx, repository, publication.Ref, publication.ResultSHA)
	}
	return git.SyncBranch(ctx, repository, publication.Ref, publication.ResultSHA, forceBranch)
}

func classifyRemotePublication(publication model.Publication, remoteSHA string, exists bool) remotePublicationState {
	if exists && sameOID(remoteSHA, publication.ResultSHA) {
		return remotePublicationResult
	}
	// The zero OID is Git's "no object" identity: a publication whose
	// recorded previous is zeroOID expects the ref to be ABSENT (branch
	// creation on an unborn target, issue #264), exactly like an empty
	// previous. A live ref can never equal the zero OID, so the exists
	// arm cannot misclassify.
	if (!exists && (publication.PreviousSHA == "" || sameOID(publication.PreviousSHA, zeroOID))) ||
		(exists && sameOID(remoteSHA, publication.PreviousSHA)) {
		return remotePublicationPrevious
	}
	return remotePublicationOther
}

func canonicalPublicationRef(kind model.RefKind, ref string) (string, error) {
	switch kind {
	case model.RefBranch:
		return "refs/heads/" + ref, nil
	case model.RefTag:
		return "refs/tags/" + ref, nil
	default:
		return "", fmt.Errorf("%w: publication ref kind", ErrInvalidInput)
	}
}

func publicationConcurrencyError(publication model.Publication, remoteSHA string, exists bool) error {
	current := "missing"
	if exists {
		current = strings.ToLower(remoteSHA)
	}
	previous := publication.PreviousSHA
	if previous == "" {
		previous = "missing"
	}
	return fmt.Errorf("upstream %s moved: expected %s, found %s", publication.Ref, previous, current)
}

func publicationAuditAction(publication model.Publication) string {
	if publication.PromotionID != "" {
		return "promotion.push"
	}
	if publication.RefKind == model.RefTag {
		return "sync.tag"
	}
	return "sync.branch"
}

func publicationAuditResource(publication model.Publication) string {
	if publication.PromotionID != "" {
		return "promotion"
	}
	return "run"
}

func publicationAuditResourceID(publication model.Publication) string {
	if publication.PromotionID != "" {
		return publication.PromotionID
	}
	return publication.RunID
}

func publicationAuditDetails(repository model.Repository, publication model.Publication) map[string]any {
	return map[string]any{
		"repo": repository.Name, "ref_kind": publication.RefKind, "ref": publication.Ref,
		"previous_sha": publication.PreviousSHA, "result_sha": publication.ResultSHA,
	}
}

// retryPublicationFailure queues a transient outage without holding a delivery
// permit or background task across backoff. Attempts are reserved before each mutation;
// a failed initial remote read also consumes one attempt.
func retryPublicationFailure(ctx context.Context, publications PublicationStore, publication model.Publication, failure error) (model.PublicationFinalization, error) {
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ctx = finalizeCtx
	if !pushretry.Transient(failure) {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationFailed, failure.Error())
	}
	if publication.Attempts >= pushretry.MaxAttempts {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationFailed, pushretry.Failure(fmt.Sprintf("all %d attempts exhausted: %v", pushretry.MaxAttempts, failure)))
	}
	next := time.Now().Add(pushretry.Delay(publication.Attempts))
	message := fmt.Sprintf(publicationRetryPrefix+"%d/%d queued for %s after transient error: %v", publication.Attempts+1, pushretry.MaxAttempts, next.UTC().Format(time.RFC3339), failure)
	updated, err := publications.UpdatePublicationAttempt(ctx, publication.ID, publication.Attempts, publication.Attempts, next, message)
	return model.PublicationFinalization{Publication: updated}, err
}

// Git I/O may consume its entire request budget. Local completion always has
// a fresh bounded storage context, including idempotent read-back paths.
func finalizePublication(ctx context.Context, publications PublicationStore, id string, status model.PublicationStatus, failure string) (model.PublicationFinalization, error) {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return publications.FinalizePublication(finalCtx, id, status, failure)
}

type mirrorBranchReader interface {
	MirrorBranchExists(context.Context, string, string) (bool, error)
}

// mirrorBranchDeleted reports a feature branch deleted on the mirror after its
// run started. Its forced publication would push the branch back upstream
// and undo the deletion (#1490).
func mirrorBranchDeleted(ctx context.Context, git PublicationGit, repository, branch string) (bool, error) {
	reader, ok := git.(mirrorBranchReader)
	if !ok {
		return false, nil
	}
	exists, err := reader.MirrorBranchExists(ctx, repository, branch)
	if err != nil {
		return false, fmt.Errorf("read mirror branch %s before publication: %w", branch, err)
	}
	return !exists, nil
}

const deletedBranchPublicationActor = "oberth:branch-deletion"

// deletedBranchPublicationSkipper finishes a deleted branch's publication
// without failing its green run.
type deletedBranchPublicationSkipper interface {
	SkipDeletedBranchPublication(context.Context, string, string, string) (model.PublicationFinalization, error)
}

// abandonDeletedBranchPublication does not push a branch deleted on the
// mirror back upstream. The run's steps passed, so the run finishes passed
// with the reason line; only the publication is abandoned (#1500).
func abandonDeletedBranchPublication(ctx context.Context, publications PublicationStore, publication model.Publication) (model.PublicationFinalization, error) {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	reason := "branch " + publication.Ref + " was deleted on the mirror; result not published, upstream left unchanged"
	var owner any = publications
	if wrapped, ok := publications.(schedulerDatabase); ok {
		owner = wrapped.SchedulerStore
	}
	if skipper, ok := owner.(deletedBranchPublicationSkipper); ok {
		return skipper.SkipDeletedBranchPublication(finalCtx, publication.ID, deletedBranchPublicationActor, reason)
	}
	if abandoner, ok := owner.(publicationAbandoner); ok {
		return abandoner.AbandonPublication(finalCtx, publication.ID, deletedBranchPublicationActor, reason)
	}
	return publications.FinalizePublication(finalCtx, publication.ID, model.PublicationFailed, reason)
}

func abandonChangedBranchPublication(ctx context.Context, publications PublicationStore, publication model.Publication) (model.PublicationFinalization, error) {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var owner any = publications
	if wrapped, ok := publications.(schedulerDatabase); ok {
		owner = wrapped.SchedulerStore
	}
	skipper, ok := owner.(deletedBranchPublicationSkipper)
	if !ok {
		return model.PublicationFinalization{}, errors.New("service: green publication abandonment unavailable")
	}
	return skipper.SkipDeletedBranchPublication(finalCtx, publication.ID, "oberth:branch-head", gitcache.ErrMirrorBranchAdvanced.Error())
}
