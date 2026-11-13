package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type PinnedSourceGit interface {
	Ensure(context.Context, string) (gitcache.Repository, error)
	ReachableFromUpstreamDefault(context.Context, string, string) (bool, error)
	ExportPinnedTree(context.Context, string, string, string, int64) error
}
type PinnedSourceRegistry interface {
	RepositoryRegistered(context.Context, string) (bool, error)
}
type PinnedSourceRecorder interface {
	RecordRunSources(context.Context, string, string, []pipeline.PinnedSource) error
}

// PinnedSourceLoader has no implicit same-organization permission. Empty policy
// denies every pair; qualified names prevent cross-upstream aliases.
type PinnedSourceLoader struct {
	git      PinnedSourceGit
	registry PinnedSourceRegistry
	policy   map[string][]string
}

func NewPinnedSourceLoader(git PinnedSourceGit, registry PinnedSourceRegistry, policyJSON string) (*PinnedSourceLoader, error) {
	if git == nil || registry == nil {
		return nil, errors.New("app: pinned sources require Git and repository registry")
	}
	if policyJSON == "" {
		policyJSON = "{}"
	}
	var policy map[string][]string
	if len(policyJSON) > 64<<10 {
		return nil, errors.New("app: source allowlist exceeds 64 KiB")
	}
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		return nil, fmt.Errorf("app: invalid source allowlist: %w", err)
	}
	if len(policy) > 256 {
		return nil, errors.New("app: source allowlist exceeds 256 consumers")
	}
	for consumer, producers := range policy {
		if !argoworkflow.ValidSourceRepository(consumer) || len(producers) > 64 {
			return nil, errors.New("app: source allowlist requires canonical repository pairs")
		}
		for _, producer := range producers {
			if !argoworkflow.ValidSourceRepository(producer) {
				return nil, errors.New("app: source allowlist requires canonical repository pairs")
			}
		}
	}
	return &PinnedSourceLoader{git: git, registry: registry, policy: policy}, nil
}

func (loader *PinnedSourceLoader) prepare(ctx context.Context, consumer, parent string, sources []pipeline.PinnedSource) ([]argojob.PinnedSourceTree, func(), error) {
	noop := func() {}
	if err := argoworkflow.ValidatePinnedSources(sources); err != nil {
		return nil, noop, err
	}
	if len(sources) == 0 {
		return nil, noop, nil
	}
	if loader == nil {
		return nil, noop, errors.New("app: pinned sources are disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, source := range sources {
		if !slices.Contains(loader.policy[consumer], source.Repository) {
			return nil, noop, fmt.Errorf("app: pinned source %s is not allowed for %s", source.Repository, consumer)
		}
		registered, err := loader.registry.RepositoryRegistered(ctx, source.Repository)
		if err != nil || !registered {
			return nil, noop, fmt.Errorf("app: pinned source %s is not a registered available repository", source.Repository)
		}
	}
	root, err := os.MkdirTemp(parent, ".pinned-sources-")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	trees := make([]argojob.PinnedSourceTree, 0, len(sources))
	for i, source := range sources {
		fail := func(reason string) ([]argojob.PinnedSourceTree, func(), error) {
			cleanup()
			return nil, noop, fmt.Errorf("app: pinned source %s@%s: %s", source.Repository, source.Commit, reason)
		}
		// Existing Git fetch and object caches retain their credential isolation,
		// receive reservations and archive gates. Never fetch from a YAML URL.
		if cached, err := loader.git.Ensure(ctx, source.Repository); err != nil || cached.Stale {
			return fail("upstream refresh failed")
		}
		reachable, err := loader.git.ReachableFromUpstreamDefault(ctx, source.Repository, source.Commit)
		if err != nil || !reachable {
			return fail("commit is missing or not reachable from the upstream default branch")
		}
		directory := filepath.Join(root, fmt.Sprint(i))
		if err := loader.git.ExportPinnedTree(ctx, source.Repository, source.Commit, directory, 128<<20); err != nil {
			return fail("exact tree export failed (unsupported entry, size limit or Git read failure)")
		}
		trees = append(trees, argojob.PinnedSourceTree{Source: source, Directory: directory})
	}
	return trees, cleanup, nil
}

func (jobs *ArgoJobs) SetPinnedSources(loader *PinnedSourceLoader, recorder PinnedSourceRecorder) {
	jobs.pinnedSources = loader
	jobs.sourceRecorder = recorder
}

func sourcesForAudit(trees []argojob.PinnedSourceTree) []pipeline.PinnedSource {
	result := make([]pipeline.PinnedSource, 0, len(trees))
	for _, tree := range trees {
		result = append(result, tree.Source)
	}
	return result
}
