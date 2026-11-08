package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type PushConfig struct {
	// DeletionQueued wakes the upstream deletion propagator; optional.
	DeletionQueued func()
	Repositories   RepositoryWriter
	Discoverer     RepositoryDiscoverer
	Git            PushGit
	Receives       ReceiveRecorder
	Branches       CIQueue
	Releases       ReleaseAdmission
	Auditor        Auditor
}

type PushIngestor struct {
	repositories RepositoryWriter
	discoverer   RepositoryDiscoverer
	git          PushGit
	receives     ReceiveRecorder
	branches     CIQueue
	releases     ReleaseAdmission
	auditor      Auditor
	accepted     receiveEventReader
	queued       func()
}

type receiveEventReader interface {
	ReceiveEvent(context.Context, string) (model.ReceiveEvent, error)
}

func NewPushIngestor(config PushConfig) (*PushIngestor, error) {
	if config.Repositories == nil || config.Discoverer == nil || config.Git == nil || config.Receives == nil {
		return nil, errors.New("service: repository store/discoverer, push Git, and receive recorder are required")
	}
	accepted, _ := config.Receives.(receiveEventReader)
	return &PushIngestor{
		repositories: databaseRepositories{config.Repositories},
		discoverer:   config.Discoverer,
		git:          config.Git,
		receives:     databaseReceives{config.Receives},
		branches:     config.Branches,
		releases:     config.Releases,
		auditor:      config.Auditor,
		accepted:     accepted,
		queued:       config.DeletionQueued,
	}, nil
}

func (ingestor *PushIngestor) Branch(ctx context.Context, push BranchPush) (PushResult, error) {
	if strings.TrimSpace(push.EventID) == "" || strings.TrimSpace(push.Repository) == "" || strings.TrimSpace(push.Branch) == "" || strings.TrimSpace(push.Actor) == "" {
		return PushResult{}, fmt.Errorf("%w: event ID, repository, branch, and actor are required", ErrInvalidInput)
	}
	if deletionOID(push.NewOID) {
		// A deletion of a registered repository needs no cache refresh:
		// Ensure fetches from the forge, and a deletion callback (including
		// its startup replay) must not wait for the network.
		repository, err := ingestor.repositories.RepositoryByName(ctx, push.Repository)
		if err == nil && repository.ArchivedAt == nil {
			return ingestor.deleteBranch(ctx, push, repository)
		}
	}
	repository, err := ingestor.ensureRepository(gitcache.WithBurstPolicy(ctx), push.Repository)
	if err != nil {
		return PushResult{}, err
	}
	if deletionOID(push.NewOID) {
		return ingestor.deleteBranch(ctx, push, repository)
	}
	if !validOID(push.NewOID) {
		return PushResult{}, fmt.Errorf("%w: branch object ID is invalid", ErrInvalidInput)
	}
	// Cache reads use the as-pushed repository input: for an org-scoped
	// push it stays fully qualified, so same-named repositories under
	// different upstreams resolve their OWN cache (issue #264).
	peeled, err := ingestor.git.PeelObject(ctx, push.Repository, strings.ToLower(push.NewOID))
	if err != nil {
		return PushResult{}, fmt.Errorf("verify accepted branch commit: %w", err)
	}
	if !sameOID(peeled.ObjectSHA, push.NewOID) || !sameOID(peeled.CommitSHA, push.NewOID) {
		return PushResult{}, errors.New("service: accepted branch commit no longer resolves exactly")
	}
	if ingestor.accepted != nil {
		event, readErr := retryDatabase(ctx, "ReceiveEvent", func(ctx context.Context) (model.ReceiveEvent, error) {
			return ingestor.accepted.ReceiveEvent(ctx, push.EventID)
		})
		if readErr == nil {
			if event.Actor != push.Actor || event.RepoID != repository.ID || event.RefKind != model.RefBranch || event.Ref != push.Branch ||
				!sameOID(event.OldSHA, push.OldOID) || !sameOID(event.ObjectSHA, peeled.CommitSHA) || !sameOID(event.CommitSHA, peeled.CommitSHA) {
				return PushResult{}, fmt.Errorf("%w: receive event ID was reused with different contents", store.ErrInvalidState)
			}
			switch event.Outcome {
			case "parked":
				return PushResult{Repository: repository, Duplicate: true, Reason: "park ref mirrored; branch CI skipped"}, nil
			case "queued":
				return ingestor.enqueueBranch(ctx, push, repository, peeled.CommitSHA, "")
			default:
				return PushResult{}, fmt.Errorf("%w: unexpected branch receive outcome", store.ErrInvalidState)
			}
		} else if !errors.Is(readErr, store.ErrNotFound) {
			return PushResult{}, readErr
		}
	}
	policy, policyErr := readPushPromotionPolicy(ctx, ingestor.git, push.Repository)
	if policyErr == nil && policy.parks(push.Branch) && push.Branch != repository.DefaultBranch && !policy.requiresBranchCI(push.Branch) {
		mirror, ok := ingestor.git.(interface {
			SyncBranch(context.Context, string, string, string) error
		})
		if !ok {
			return PushResult{}, refuse(ErrUnavailable, "park branch mirror")
		}
		if err := auditedGitMutation(ctx, ingestor.auditor, push.Actor, "branch.park", "repository", push.Repository,
			map[string]any{"branch": push.Branch, "sha": peeled.CommitSHA, "event_id": push.EventID},
			func() error { return mirror.SyncBranch(ctx, push.Repository, push.Branch, peeled.CommitSHA) }); err != nil {
			return PushResult{}, fmt.Errorf("mirror park branch: %w", err)
		}
		duplicate, err := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(push.EventID, push.Actor, repository.ID, model.RefBranch, push.Branch, push.OldOID, peeled.CommitSHA, peeled.CommitSHA, "parked"))
		return PushResult{Repository: repository, Duplicate: duplicate, Reason: "park ref mirrored; branch CI skipped"}, err
	}
	reason := ""
	if policyErr != nil {
		// Keep the existing reason line bounded and printable. The error is
		// server-side policy/read metadata, never candidate authorization.
		reason = parkFallbackPrefix + boundedParkPolicyText(policyErr.Error()) + ")"
	}
	return ingestor.enqueueBranch(ctx, push, repository, peeled.CommitSHA, reason)
}

// deleteBranch acknowledges a branch deletion receive-pack already applied to
// the mirror. It records the receive event and a pending upstream deletion in
// one transaction and returns: upstream propagation is asynchronous, so a
// slow or unreachable forge never holds the receive, its replay, or startup.
// It makes no Git call: receive finalization already wrote the deletion
// marker under the repository lock, and callbacks run under the receive lock.
func (ingestor *PushIngestor) deleteBranch(ctx context.Context, push BranchPush, repository model.Repository) (PushResult, error) {
	if repository.DefaultBranch == "" || push.Branch == repository.DefaultBranch {
		return PushResult{}, fmt.Errorf("%w: cannot delete the default branch", ErrInvalidInput)
	}
	duplicate, err := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(push.EventID, push.Actor, repository.ID, model.RefBranch, push.Branch, push.OldOID, "", "", "deleted"))
	if err != nil {
		return PushResult{}, err
	}
	if ingestor.queued != nil {
		ingestor.queued()
	}
	return PushResult{Repository: repository, Deleted: true, Duplicate: duplicate, Reason: "branch deleted on the mirror; upstream deletion queued; no CI run"}, nil
}

func (ingestor *PushIngestor) enqueueBranch(ctx context.Context, push BranchPush, repository model.Repository, sha, reason string) (PushResult, error) {
	if ingestor.branches == nil {
		return PushResult{}, refuse(ErrUnavailable, "CI queue")
	}
	enqueued, err := ingestor.branches.EnqueueCI(ctx, CIRequest{PriorityRequest: push.PriorityRequest,
		Assist:     push.Assist,
		Reason:     reason,
		EventID:    push.EventID,
		Repository: repository,
		Branch:     push.Branch,
		OldSHA:     push.OldOID,
		SHA:        strings.ToLower(sha),
		Actor:      push.Actor,
		Trigger:    "branch",
		TestedSHA:  strings.ToLower(sha),
	})
	if err != nil {
		return PushResult{}, fmt.Errorf("enqueue accepted branch commit: %w", err)
	}
	return pushRunResult(repository, enqueued), nil
}

func (ingestor *PushIngestor) Tag(ctx context.Context, push TagPush) (PushResult, error) {
	if strings.TrimSpace(push.EventID) == "" || strings.TrimSpace(push.Repository) == "" || strings.TrimSpace(push.Tag) == "" || strings.TrimSpace(push.Actor) == "" {
		return PushResult{}, fmt.Errorf("%w: event ID, repository, tag, and actor are required", ErrInvalidInput)
	}
	repository, err := ingestor.ensureRepository(ctx, push.Repository)
	if err != nil {
		return PushResult{}, err
	}
	if deletionOID(push.NewOID) {
		if !validOID(push.OldOID) {
			return PushResult{}, fmt.Errorf("%w: deleted tag object ID is invalid", ErrInvalidInput)
		}
		peeled, peelErr := ingestor.git.PeelObject(ctx, push.Repository, strings.ToLower(push.OldOID))
		if peelErr != nil {
			return PushResult{}, fmt.Errorf("peel rejected tag deletion: %w", peelErr)
		}
		duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(
			push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag,
			push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_rejected",
		))
		if recordErr != nil {
			return PushResult{}, recordErr
		}
		return PushResult{Repository: repository, Duplicate: duplicate, Reason: "release tags are immutable"}, nil
	}
	if ingestor.releases == nil {
		return PushResult{}, refuse(ErrUnavailable, "release admission")
	}
	if !validOID(push.NewOID) {
		return PushResult{}, fmt.Errorf("%w: tag object ID is invalid", ErrInvalidInput)
	}
	if !validOID(push.ReleaseAdmissionSHA) {
		return PushResult{}, fmt.Errorf("%w: release admission object ID is invalid", ErrInvalidInput)
	}
	peeled, err := ingestor.git.PeelObject(ctx, push.Repository, strings.ToLower(push.NewOID))
	if err != nil {
		return PushResult{}, fmt.Errorf("peel accepted tag: %w", err)
	}
	if !sameOID(peeled.ObjectSHA, push.NewOID) || !validOID(peeled.CommitSHA) {
		return PushResult{}, errors.New("service: peeled tag did not resolve to a commit")
	}
	// Release provenance requires an annotated tag object. A lightweight tag
	// is a commit OID, not a tag object; accepting it would grant release-tier
	// execution without the annotated-tag provenance the contract requires.
	if peeled.ObjectType != "tag" {
		duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(
			push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag,
			push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_rejected",
		))
		if recordErr != nil {
			return PushResult{}, recordErr
		}
		return PushResult{Repository: repository, Duplicate: duplicate, Reason: "release tags must be annotated (lightweight tags are not admitted)"}, nil
	}
	if validOID(push.OldOID) && !sameOID(push.OldOID, peeled.ObjectSHA) {
		duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(
			push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag,
			push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_rejected",
		))
		if recordErr != nil {
			return PushResult{}, recordErr
		}
		return PushResult{Repository: repository, Duplicate: duplicate, Reason: "release tags are immutable"}, nil
	}
	reachable, err := ingestor.git.ReleaseReachable(ctx, push.Repository, strings.ToLower(peeled.CommitSHA), strings.ToLower(push.ReleaseAdmissionSHA))
	if err != nil {
		return PushResult{}, fmt.Errorf("check release ancestry: %w", err)
	}
	if !reachable {
		duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag, push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_rejected"))
		if recordErr != nil {
			return PushResult{}, recordErr
		}
		return PushResult{Repository: repository, Duplicate: duplicate, Reason: ErrReleaseUnreachable.Error()}, nil
	}
	remoteObject, exists, err := ingestor.git.RemoteRef(ctx, push.Repository, "refs/tags/"+push.Tag)
	if err != nil {
		return PushResult{}, fmt.Errorf("preflight upstream release tag: %w", err)
	}
	if exists {
		if sameOID(remoteObject, peeled.ObjectSHA) {
			duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(
				push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag,
				push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_delivered",
			))
			if recordErr != nil {
				return PushResult{}, recordErr
			}
			return PushResult{Repository: repository, Duplicate: duplicate, Reason: "tag already delivered upstream"}, nil
		}
		duplicate, recordErr := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(
			push.EventID, push.Actor, repository.ID, model.RefTag, push.Tag,
			push.OldOID, peeled.ObjectSHA, peeled.CommitSHA, "release_rejected",
		))
		if recordErr != nil {
			return PushResult{}, recordErr
		}
		return PushResult{
			Repository: repository, Duplicate: duplicate,
			Reason: "upstream tag already exists at a different object",
		}, nil
	}
	enqueued, err := ingestor.releases.AdmitRelease(ctx, ReleaseRequest{PriorityRequest: push.PriorityRequest,
		EventID:    push.EventID,
		Repository: repository,
		Tag:        push.Tag,
		OldSHA:     push.OldOID,
		ObjectSHA:  strings.ToLower(peeled.ObjectSHA),
		CommitSHA:  strings.ToLower(peeled.CommitSHA),
		Actor:      push.Actor,
	})
	if err != nil {
		return PushResult{}, fmt.Errorf("admit reachable release tag: %w", err)
	}
	result := pushRunResult(repository, enqueued)
	result.Admitted = true
	return result, nil
}

func (ingestor *PushIngestor) ensureRepository(ctx context.Context, name string) (model.Repository, error) {
	if repository, err := ingestor.repositories.RepositoryByName(ctx, name); err == nil && repository.ArchivedAt != nil {
		return model.Repository{}, store.ErrRepositoryArchived
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return model.Repository{}, err
	}
	cache, err := ingestor.git.Ensure(ctx, name)
	if err != nil {
		return model.Repository{}, fmt.Errorf("ensure repository cache: %w", err)
	}
	repository, lookupErr := ingestor.repositories.RepositoryByName(ctx, name)
	if lookupErr == nil {
		if cache.DefaultBranch != "" && cache.DefaultBranch != repository.DefaultBranch {
			repository, err = ingestor.repositories.SetRepositoryDefaultBranch(ctx, repository.ID, cache.DefaultBranch)
			if err != nil {
				return model.Repository{}, fmt.Errorf("persist changed upstream default branch: %w", err)
			}
		}
		return repository, nil
	}
	if !errors.Is(lookupErr, store.ErrNotFound) {
		return model.Repository{}, fmt.Errorf("look up repository: %w", lookupErr)
	}
	discovery, err := ingestor.discoverer.DiscoverRepository(ctx, name)
	if err != nil {
		return model.Repository{}, fmt.Errorf("discover repository metadata: %w", err)
	}
	// Discovery returns the BARE repository name; the pushed path may be
	// org- or upstream-qualified (the qualifiers routed upstream selection
	// and the cache layout, but the catalog registers the bare name).
	_, _, bareName, parseErr := gitcache.ParseRepoPath(name)
	if parseErr != nil {
		return model.Repository{}, fmt.Errorf("parse repository path: %w", parseErr)
	}
	if strings.TrimSpace(discovery.Name) == "" {
		discovery.Name = bareName
	}
	discovery.DefaultBranch = cache.DefaultBranch
	if discovery.Name != bareName || discovery.UpstreamID <= 0 || strings.TrimSpace(discovery.DefaultBranch) == "" {
		return model.Repository{}, errors.New("service: repository discovery is incomplete")
	}
	repository, createErr := ingestor.repositories.CreateRepository(ctx, discovery)
	if createErr == nil {
		return repository, nil
	}
	// A concurrent first push may have won the unique-name insert.
	repository, lookupErr = ingestor.repositories.RepositoryByName(ctx, name)
	if lookupErr == nil {
		return repository, nil
	}
	return model.Repository{}, fmt.Errorf("create discovered repository: %w", createErr)
}

func pushRunResult(repository model.Repository, enqueued model.EnqueueRunResult) PushResult {
	if enqueued.DeferredReason != "" {
		return PushResult{Repository: repository, Duplicate: enqueued.Duplicate, Reason: enqueued.DeferredReason}
	}
	run := presentationRun(enqueued.Run)
	return PushResult{Repository: repository, Run: &run, Cancellations: enqueued.Cancellations, SupersededCount: enqueued.SupersededCount, Duplicate: enqueued.Duplicate}
}

func receiveSpec(id, actor string, repoID int64, kind model.RefKind, ref, oldSHA, objectSHA, commitSHA, outcome string) model.ReceiveEventSpec {
	return model.ReceiveEventSpec{
		ID: id, Actor: actor, RepoID: repoID, RefKind: kind, Ref: ref, OldSHA: oldSHA,
		ObjectSHA: objectSHA, CommitSHA: commitSHA, Outcome: outcome,
	}
}

func deletionOID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character != '0' {
			return false
		}
	}
	return true
}

func validOID(value string) bool { return gitoid.ValidTrimmed(value) }

// zeroOID is Git's canonical "no object" identity — the old-oid receive-pack
// advertises when a ref is created. Promotion rows use it as the planned
// base of an unborn target (issue #264) so the append-only store still
// records a concrete plan while delivery keeps creation semantics.
const zeroOID = "0000000000000000000000000000000000000000"

func sameOID(first, second string) bool { return gitoid.Same(first, second) }
