package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type FragmentLoader interface {
	Load(ctx context.Context, key argoworkflow.FragmentKey) (argoworkflow.Fragment, error)
}

type FragmentBlobs interface {
	TagSHA(ctx context.Context, input, tag string) (string, error)
	// ReachableFromUpstreamDefault is the fragment reachability gate (#213):
	// a fragment tag's commit must be an ancestor of its source repository's
	// upstream default branch before any pipeline may inline it. A tag is
	// creation-only but may point at a commit that never went through
	// green-and-publish; without this gate such a tag would inject unreviewed,
	// never-published templates into a consuming repository's pipeline.
	ReachableFromUpstreamDefault(ctx context.Context, input, commit string) (bool, error)
	ReadBlob(ctx context.Context, input, sha, file string, limit int) ([]byte, error)
}

type FragmentRegistry interface {
	RepositoryRegistered(ctx context.Context, name string) (bool, error)
}

type GitFragmentLoader struct {
	blobs     FragmentBlobs
	registry  FragmentRegistry
	allowlist map[string]bool
}

func NewGitFragmentLoader(blobs FragmentBlobs, registry FragmentRegistry, allowlist []string) (*GitFragmentLoader, error) {
	if blobs == nil || registry == nil {
		return nil, errors.New("app: fragment loading requires a git cache and a repository registry")
	}
	loader := &GitFragmentLoader{blobs: blobs, registry: registry}
	if len(allowlist) > 0 {
		loader.allowlist = make(map[string]bool, len(allowlist))
		for _, entry := range allowlist {
			key, err := argoworkflow.ParseFragmentRef(entry + "@v0")
			if err != nil {
				return nil, fmt.Errorf("app: fragment allowlist entry %q: %w", entry, err)
			}
			loader.allowlist[key.Repo] = true
		}
	}
	return loader, nil
}

func (loader *GitFragmentLoader) Load(ctx context.Context, key argoworkflow.FragmentKey) (argoworkflow.Fragment, error) {
	if loader.allowlist != nil && !loader.allowlist[key.Repo] {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s is not in the fragment allowlist", key)
	}
	_, _, name, err := gitcache.ParseRepoPath(key.Repo)
	if err != nil {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s: %w", key, err)
	}
	registered, err := loader.registry.RepositoryRegistered(ctx, name)
	if err != nil {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s: %w", key, err)
	}
	if !registered {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s names a repository this server does not host", key)
	}
	sha, err := loader.blobs.TagSHA(ctx, key.Repo, key.Version)
	if err != nil {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s: %w", key, err)
	}
	// Reachability gate (#213). Enforced for every trigger tier: the read
	// below serves template content into another repository's pipeline, so
	// the source commit must meet the same bar release admission applies to
	// the consumer's own code — reachable from the upstream default branch,
	// meaning it went through green-and-publish on its own repository.
	reachable, err := loader.blobs.ReachableFromUpstreamDefault(ctx, key.Repo, sha)
	if err != nil {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s: %w", key, err)
	}
	if !reachable {
		return argoworkflow.Fragment{}, fmt.Errorf(
			"app: fragment %s resolves to commit %s, which is not reachable from its repository's upstream default branch; only published versions may be inlined",
			key, sha)
	}
	source, err := loader.blobs.ReadBlob(ctx, key.Repo, sha, argoworkflow.FragmentFile, argoworkflow.MaxSourceBytes)
	if err != nil {
		return argoworkflow.Fragment{}, fmt.Errorf("app: fragment %s: %w", key, err)
	}
	return argoworkflow.Fragment{Key: key, SHA: sha, Source: source}, nil
}

func loadFragments(ctx context.Context, loader FragmentLoader, source []byte) (map[argoworkflow.FragmentKey]argoworkflow.Fragment, error) {
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return nil, err
	}
	keys, err := argoworkflow.FragmentRefs(workflow)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if loader == nil {
		return nil, fmt.Errorf("app: the pipeline references fragment %s but this server has no fragment loader configured", keys[0])
	}
	fragments := make(map[argoworkflow.FragmentKey]argoworkflow.Fragment, len(keys))
	for _, key := range keys {
		fragment, err := loader.Load(ctx, key)
		if err != nil {
			return nil, err
		}
		fragments[key] = fragment
	}
	return fragments, nil
}
