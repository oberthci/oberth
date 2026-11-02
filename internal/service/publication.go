package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	forceBranch := publication.PromotionID == "" && publication.RefKind == model.RefBranch && publication.Ref != repository.DefaultBranch
	if forceBranch {
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
	if mutationErr == nil {
		return finalizePublication(ctx, publications, publication.ID, model.PublicationDelivered, "")
	}

	// A failed push or audit response can be ambiguous: the remote may already
	// hold the exact result. Read it before deciding whether the operation is red.
	resume := suspendDispatch(ctx)
	remoteSHA, exists, verifyErr := git.RemoteRef(ctx, repositoryInput, ref)
	resume()
	if verifyErr != nil {
		failure := errors.Join(
			fmt.Errorf("publication %s mutation was ambiguous: %w", publication.ID, mutationErr),
			fmt.Errorf("verify remote ref after ambiguous publication: %w", verifyErr),
		)
		return retryPublicationFailure(ctx, publications, publication, failure)
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
	// The default branch uses a non-forced push (PushPromotion semantics) so
	// a direct push to main cannot silently overwrite a just-delivered
	// promotion. Feature branches keep force-push behavior (#432).
	if !forceBranch {
		return git.PushPromotion(ctx, repository, publication.Ref, publication.ResultSHA)
	}
	return git.SyncBranch(ctx, repository, publication.Ref, publication.ResultSHA)
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
// permit or worker across backoff. Attempts are reserved before each mutation;
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
