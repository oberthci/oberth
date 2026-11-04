package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const promotionPolicyFile = ".oberth/promotion.yaml"
const defaultParkPrefix = "refs/heads/park/"

type promotionPolicy struct {
	ParkRefPrefix        string   `yaml:"park_ref_prefix"`
	AllowWithoutBranchCI bool     `yaml:"allow_without_branch_ci"`
	RequireBranchCI      []string `yaml:"require_branch_ci"`
}

type parkSourceResolver interface {
	ParkSource(context.Context, string, string, string) (string, error)
}

type promotionPolicyReader interface {
	ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error)
}

// Only the fresh upstream default may authorize skipping branch CI. In
// particular, neither a parked candidate nor a locally pushed main is policy.
func readPromotionPolicy(ctx context.Context, git any, repo string) (promotionPolicy, error) {
	ctx, cancel := context.WithTimeout(gitcache.WithFreshPolicy(ctx), 30*time.Second)
	defer cancel()
	return readPromotionPolicyContext(ctx, git, repo)
}

// Push admission can reuse the recent upstream snapshot. Any error falls back
// to ordinary CI; only promotion authorization requires a fresh successful read.
func readPushPromotionPolicy(ctx context.Context, git any, repo string) (promotionPolicy, error) {
	ctx, cancel := context.WithTimeout(gitcache.WithBurstPolicy(ctx), 2*time.Second)
	defer cancel()
	return readPromotionPolicyContext(ctx, git, repo)
}

func readPromotionPolicyContext(ctx context.Context, git any, repo string) (promotionPolicy, error) {
	p := promotionPolicy{ParkRefPrefix: defaultParkPrefix}
	reader, ok := git.(promotionPolicyReader)
	if !ok {
		return p, nil
	}
	_, body, exists, err := reader.ReadDefaultBlob(ctx, repo, promotionPolicyFile, 16384)
	if err != nil {
		return p, fmt.Errorf("read promotion policy: %w", err)
	}
	if !exists {
		return p, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	if err := decoder.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid promotion policy: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return p, errors.New("promotion policy must contain exactly one document")
	}
	if !strings.HasPrefix(p.ParkRefPrefix, "refs/heads/") || !strings.HasSuffix(p.ParkRefPrefix, "/") || gitcache.ValidateBranch(strings.TrimSuffix(strings.TrimPrefix(p.ParkRefPrefix, "refs/heads/"), "/")) != nil {
		return p, errors.New("park_ref_prefix must be a branch namespace under refs/heads/ with a trailing slash")
	}
	for _, pattern := range p.RequireBranchCI {
		if strings.TrimSpace(pattern) == "" {
			return p, errors.New("require_branch_ci patterns must not be empty")
		}
		if _, err := path.Match(pattern, "main"); err != nil {
			return p, fmt.Errorf("invalid require_branch_ci pattern: %w", err)
		}
	}
	return p, nil
}
func (p promotionPolicy) requiresBranchCI(branch string) bool {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	for _, pattern := range p.RequireBranchCI {
		if ok, _ := path.Match(strings.TrimPrefix(pattern, "refs/heads/"), branch); ok {
			return true
		}
	}
	return false
}
func (p promotionPolicy) parks(branch string) bool {
	return strings.HasPrefix("refs/heads/"+branch, p.ParkRefPrefix)
}

func greenBranchCandidate(candidate model.Run) bool {
	return candidate.RefKind == model.RefBranch && candidate.Status == model.RunPassed && sameOID(candidate.TestedSHA, candidate.SHA) && (candidate.Trigger == "" || candidate.Trigger == "branch" || candidate.Trigger == "schedule")
}

func refuseRedBranchSource(run model.Run) error {
	if run.RefKind == model.RefBranch && run.Trigger != "promotion" && !run.Release && (run.Status == model.RunFailed || run.Status == model.RunInterrupted) {
		return refuse(ErrInvalidInput, "promotion source must be the exactly tested green branch commit; promotion source has a red branch run %s (%s), the newest run for %s; rerun or fix it", run.ID, run.Status, run.SHA)
	}
	return nil
}

// Resolve parked SHAs without inventing a green run or borrowing a promotion
// run as branch evidence. Repository resolution still uses the scoped store.
func (service *API) resolvePromotionSource(ctx context.Context, repo, sha string) (model.Repository, model.Run, error) {
	if repo == "" {
		repository, run, err := service.resolveRun(ctx, repo, sha)
		if err == nil {
			if redErr := refuseRedBranchSource(run); redErr != nil {
				return model.Repository{}, model.Run{}, redErr
			}
			if greenBranchCandidate(run) {
				return repository, run, nil
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return model.Repository{}, model.Run{}, err
		}
		return model.Repository{}, model.Run{}, refuse(ErrInvalidInput, "promotion source must be the exactly tested green branch commit; parked promotion source requires repo")
	}
	return service.resolveRunWith(ctx, repo, sha, func(ctx context.Context, id int64, sha string) (model.Run, error) {
		run, err := service.runs.ResolveRun(ctx, id, sha)
		if err == nil {
			if redErr := refuseRedBranchSource(run); redErr != nil {
				return model.Run{}, redErr
			}
		}
		if err == nil && greenBranchCandidate(run) {
			return run, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return model.Run{}, err
		}
		repository, lookupErr := service.repositoryByID(ctx, id)
		if lookupErr != nil {
			return model.Run{}, lookupErr
		}
		if repository.ArchivedAt != nil {
			if repo == "" {
				return model.Run{}, store.ErrNotFound
			}
			return model.Run{}, store.ErrRepositoryArchived
		}
		policy, policyErr := readPromotionPolicy(ctx, service.git, service.cacheInputFor(ctx, repository))
		if policyErr != nil {
			// An uncached repository has no accepted public park refs. Unlike an
			// unavailable upstream of a cached repository, it cannot be an ambiguous
			// match while resolving a SHA across the catalog.
			if errors.Is(policyErr, gitcache.ErrPolicyRepositoryNotCached) && errors.Is(err, store.ErrNotFound) {
				return model.Run{}, err
			}
			return model.Run{}, policyErr
		}
		resolver, ok := service.git.(parkSourceResolver)
		if ok {
			ref, parkErr := resolver.ParkSource(ctx, service.cacheInputFor(ctx, repository), sha, policy.ParkRefPrefix)
			if parkErr != nil {
				return model.Run{}, parkErr
			}
			if ref != "" && policy.parks(ref) && ref != repository.DefaultBranch {
				return model.Run{RepoID: id, RefKind: model.RefBranch, Ref: ref, SHA: sha}, nil
			}
		}
		return run, err
	})
}
