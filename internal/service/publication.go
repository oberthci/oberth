package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

type remotePublicationState uint8

const (
	remotePublicationOther remotePublicationState = iota
	remotePublicationPrevious
	remotePublicationResult
)

// deliverPublication classifies an exact durable intent against the remote
// before deciding whether a mutation is safe. An ambiguous transport failure
// is classified by one final ref read. Every bounded attempt terminalizes;
// only an actual process crash can leave an intent for one-shot startup recovery.
func deliverPublication(
	ctx context.Context,
	publications PublicationStore,
	git PublicationGit,
	auditor Auditor,
	repository model.Repository,
	publication model.Publication,
) (model.PublicationFinalization, error) {
	if publication.Status != model.PublicationPending || publication.RepoID != repository.ID {
		return model.PublicationFinalization{}, errors.New("service: publication does not match repository or is already terminal")
	}
	ref, err := canonicalPublicationRef(publication.RefKind, publication.Ref)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	remoteSHA, exists, err := git.RemoteRef(ctx, repository.Name, ref)
	if err != nil {
		failure := fmt.Errorf("read remote ref for publication %s: %w", publication.ID, err)
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed, failure.Error())
	}
	forceBranch := publication.PromotionID == "" && publication.RefKind == model.RefBranch
	if forceBranch {
		if exists && sameOID(remoteSHA, publication.ResultSHA) {
			return publications.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
		}
		return attemptPublicationMutation(ctx, publications, git, auditor, repository, publication, ref, true)
	}
	if !publication.PreviousKnown {
		publication, err = publications.SetPublicationPredecessor(ctx, publication.ID, remoteSHA, exists)
		if err != nil {
			return model.PublicationFinalization{}, err
		}
	}
	switch classifyRemotePublication(publication, remoteSHA, exists) {
	case remotePublicationResult:
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	case remotePublicationOther:
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed,
			publicationConcurrencyError(publication, remoteSHA, exists).Error())
	}
	return attemptPublicationMutation(ctx, publications, git, auditor, repository, publication, ref, false)
}

func attemptPublicationMutation(
	ctx context.Context,
	publications PublicationStore,
	git PublicationGit,
	auditor Auditor,
	repository model.Repository,
	publication model.Publication,
	ref string,
	forceBranch bool,
) (model.PublicationFinalization, error) {
	mutationErr := auditedGitMutation(ctx, auditor, publication.Actor,
		publicationAuditAction(publication), publicationAuditResource(publication), publicationAuditResourceID(publication),
		publicationAuditDetails(repository, publication),
		func() error { return mutatePublication(ctx, git, repository.Name, publication) })
	if mutationErr == nil {
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	}

	// A failed push or audit response can be ambiguous: the remote may already
	// hold the exact result. Read it before deciding whether the operation is red.
	remoteSHA, exists, verifyErr := git.RemoteRef(ctx, repository.Name, ref)
	if verifyErr != nil {
		failure := errors.Join(
			fmt.Errorf("publication %s mutation was ambiguous: %w", publication.ID, mutationErr),
			fmt.Errorf("verify remote ref after ambiguous publication: %w", verifyErr),
		)
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed, failure.Error())
	}
	switch classifyRemotePublication(publication, remoteSHA, exists) {
	case remotePublicationResult:
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	case remotePublicationPrevious:
		// The bounded publication attempt did not move the ref. Terminalize it;
		// FAB has no background publication loop, and a later explicit sync
		// or promotion is the retry boundary.
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed, mutationErr.Error())
	default:
		if forceBranch {
			return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed, mutationErr.Error())
		}
		failure := errors.Join(mutationErr, publicationConcurrencyError(publication, remoteSHA, exists))
		return publications.FinalizePublication(ctx, publication.ID, model.PublicationFailed, failure.Error())
	}
}

func mutatePublication(ctx context.Context, git PublicationGit, repository string, publication model.Publication) error {
	if publication.PromotionID != "" {
		return git.PushPromotion(ctx, repository, publication.Ref, publication.ResultSHA)
	}
	if publication.RefKind == model.RefTag {
		return git.SyncTag(ctx, repository, publication.Ref, publication.ResultSHA)
	}
	return git.SyncBranch(ctx, repository, publication.Ref, publication.ResultSHA)
}

func classifyRemotePublication(publication model.Publication, remoteSHA string, exists bool) remotePublicationState {
	if exists && sameOID(remoteSHA, publication.ResultSHA) {
		return remotePublicationResult
	}
	if (!exists && publication.PreviousSHA == "") || (exists && sameOID(remoteSHA, publication.PreviousSHA)) {
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
