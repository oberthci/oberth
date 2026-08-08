package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type PushConfig struct {
	Repositories RepositoryWriter
	Discoverer   RepositoryDiscoverer
	Git          PushGit
	Receives     ReceiveRecorder
	Branches     CIQueue
	Releases     ReleaseAdmission
}

type PushIngestor struct {
	repositories RepositoryWriter
	discoverer   RepositoryDiscoverer
	git          PushGit
	receives     ReceiveRecorder
	branches     CIQueue
	releases     ReleaseAdmission
}

func NewPushIngestor(config PushConfig) (*PushIngestor, error) {
	if config.Repositories == nil || config.Discoverer == nil || config.Git == nil || config.Receives == nil {
		return nil, errors.New("service: repository store/discoverer, push Git, and receive recorder are required")
	}
	return &PushIngestor{
		repositories: config.Repositories,
		discoverer:   config.Discoverer,
		git:          config.Git,
		receives:     config.Receives,
		branches:     config.Branches,
		releases:     config.Releases,
	}, nil
}

func (ingestor *PushIngestor) Branch(ctx context.Context, push BranchPush) (PushResult, error) {
	if strings.TrimSpace(push.EventID) == "" || strings.TrimSpace(push.Repository) == "" || strings.TrimSpace(push.Branch) == "" || strings.TrimSpace(push.Actor) == "" {
		return PushResult{}, fmt.Errorf("%w: event ID, repository, branch, and actor are required", ErrInvalidInput)
	}
	repository, err := ingestor.ensureRepository(ctx, push.Repository)
	if err != nil {
		return PushResult{}, err
	}
	if deletionOID(push.NewOID) {
		duplicate, err := ingestor.receives.RecordReceiveEvent(ctx, receiveSpec(push.EventID, push.Actor, repository.ID, model.RefBranch, push.Branch, push.OldOID, "", "", "deleted"))
		if err != nil {
			return PushResult{}, err
		}
		return PushResult{Repository: repository, Deleted: true, Duplicate: duplicate, Reason: "branch deletion requires no CI run"}, nil
	}
	if ingestor.branches == nil {
		return PushResult{}, fmt.Errorf("%w: CI queue", ErrUnavailable)
	}
	if !validOID(push.NewOID) {
		return PushResult{}, fmt.Errorf("%w: branch object ID is invalid", ErrInvalidInput)
	}
	peeled, err := ingestor.git.PeelObject(ctx, repository.Name, strings.ToLower(push.NewOID))
	if err != nil {
		return PushResult{}, fmt.Errorf("verify accepted branch commit: %w", err)
	}
	if !sameOID(peeled.ObjectSHA, push.NewOID) || !sameOID(peeled.CommitSHA, push.NewOID) {
		return PushResult{}, errors.New("service: accepted branch commit no longer resolves exactly")
	}
	enqueued, err := ingestor.branches.EnqueueCI(ctx, CIRequest{
		EventID:    push.EventID,
		Repository: repository,
		Branch:     push.Branch,
		OldSHA:     push.OldOID,
		SHA:        strings.ToLower(peeled.CommitSHA),
		Actor:      push.Actor,
		Trigger:    "branch",
		TestedSHA:  strings.ToLower(peeled.CommitSHA),
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
		peeled, peelErr := ingestor.git.PeelObject(ctx, repository.Name, strings.ToLower(push.OldOID))
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
		return PushResult{}, fmt.Errorf("%w: release admission", ErrUnavailable)
	}
	if !validOID(push.NewOID) {
		return PushResult{}, fmt.Errorf("%w: tag object ID is invalid", ErrInvalidInput)
	}
	if !validOID(push.ReleaseAdmissionSHA) {
		return PushResult{}, fmt.Errorf("%w: release admission object ID is invalid", ErrInvalidInput)
	}
	peeled, err := ingestor.git.PeelObject(ctx, repository.Name, strings.ToLower(push.NewOID))
	if err != nil {
		return PushResult{}, fmt.Errorf("peel accepted tag: %w", err)
	}
	if !sameOID(peeled.ObjectSHA, push.NewOID) || !validOID(peeled.CommitSHA) {
		return PushResult{}, errors.New("service: peeled tag did not resolve to a commit")
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
	reachable, err := ingestor.git.ReleaseReachable(ctx, repository.Name, strings.ToLower(peeled.CommitSHA), strings.ToLower(push.ReleaseAdmissionSHA))
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
	remoteObject, exists, err := ingestor.git.RemoteRef(ctx, repository.Name, "refs/tags/"+push.Tag)
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
	enqueued, err := ingestor.releases.AdmitRelease(ctx, ReleaseRequest{
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
	if strings.TrimSpace(discovery.Name) == "" {
		discovery.Name = name
	}
	discovery.DefaultBranch = cache.DefaultBranch
	if discovery.Name != name || discovery.UpstreamID <= 0 || strings.TrimSpace(discovery.DefaultBranch) == "" {
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
	run := enqueued.Run
	return PushResult{Repository: repository, Run: &run, Cancellations: enqueued.Cancellations, Duplicate: enqueued.Duplicate}
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

func sameOID(first, second string) bool { return gitoid.Same(first, second) }
