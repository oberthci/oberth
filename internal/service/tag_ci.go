package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func (ingestor *PushIngestor) isReleaseTag(ctx context.Context, repo model.Repository, tag string) (bool, error) {
	if model.IsReleaseVersionTag(tag) {
		return true, nil
	}
	if ingestor.releaseTagRoute != nil {
		if ingestor.qualifiedRepo == nil {
			return false, refuse(ErrUnavailable, "canonical repository identity")
		}
		canonical, err := ingestor.qualifiedRepo(ctx, repo.ID)
		if err != nil {
			return false, err
		}
		return ingestor.releaseTagRoute(canonical, tag)
	}
	return false, nil
}

// PrepareTagCI verifies the same fresh default-branch ancestry required for a
// received tag before tag_after changes an upstream ref. The returned snapshot
// is retained through the shared Tag admission, not replaced by the target or
// a client-writable mirror branch.
func (ingestor *PushIngestor) PrepareTagCI(ctx context.Context, push TagPush, commit string) (TagPush, error) {
	if err := gitcache.ValidateTag(push.Tag); err != nil || strings.HasPrefix(push.Tag, "refs/") || !validOID(commit) {
		return TagPush{}, refuse(ErrInvalidInput, "invalid tag CI request")
	}
	if model.IsReleaseVersionTag(push.Tag) {
		return TagPush{}, refuse(ErrInvalidInput, "tag_after refuses release-version names")
	}
	if ingestor.branches == nil {
		return TagPush{}, refuse(ErrUnavailable, "tag CI admission")
	}
	repo, err := ingestor.ensureRepository(ctx, push.Repository)
	if err != nil {
		return TagPush{}, err
	}
	release, err := ingestor.isReleaseTag(ctx, repo, push.Tag)
	if err != nil {
		return TagPush{}, err
	}
	if release {
		return TagPush{}, refuse(ErrInvalidInput, "tag_after refuses administrator release routes")
	}
	head, exists, err := ingestor.git.RemoteRef(ctx, push.Repository, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return TagPush{}, fmt.Errorf("read tag CI default branch: %w", err)
	}
	if !exists || !validOID(head) {
		return TagPush{}, refuse(ErrInvalidInput, "tag CI requires an upstream default branch")
	}
	reachable, err := ingestor.git.ReleaseReachable(ctx, push.Repository, commit, head)
	if err != nil {
		return TagPush{}, fmt.Errorf("verify tag CI default-branch ancestry: %w", err)
	}
	if !reachable {
		return TagPush{}, ErrReleaseUnreachable
	}
	push.ReleaseAdmissionSHA = strings.ToLower(head)
	return push, nil
}
