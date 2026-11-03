package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/retirement"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type RetirementCatalog interface {
	ListRepositories(context.Context) ([]model.Repository, error)
	Upstream(context.Context, int64) (model.Upstream, error)
	ActiveWorkflowCapability(context.Context, int64) (*argoworkflow.Capability, error)
	LegacyRetirementBlockers(context.Context) ([]string, error)
}

type RetirementGit interface {
	RetirementSources(context.Context, string) ([]gitcache.RetirementSource, error)
	Ensure(context.Context, string) (gitcache.Repository, error)
}

type LegacyRemovalCheck struct {
	Catalog   RetirementCatalog
	Git       RetirementGit
	Fragments FragmentLoader
}

// Check is read-only with respect to application state. Fresh Git fetches may
// update the cache. It never executes repository code or marks cleanup complete.
func (c LegacyRemovalCheck) Check(ctx context.Context) error {
	if err := c.inspect(ctx); err != nil {
		return fmt.Errorf("legacy execution removal refused: %w", err)
	}
	return nil
}

func (c LegacyRemovalCheck) inspect(ctx context.Context) error {
	if c.Catalog == nil || c.Git == nil {
		return errors.New("legacy retirement inventory unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := c.checkObligations(ctx); err != nil {
		return err
	}
	repositories, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		return fmt.Errorf("read registered repositories: %w", err)
	}
	if len(repositories) > 1000 {
		return errors.New("legacy retirement inventory exceeds 1000 registered repositories")
	}
	for _, repo := range repositories {
		upstream, err := c.Catalog.Upstream(ctx, repo.UpstreamID)
		if err != nil {
			return fmt.Errorf("repository %s: read upstream: %w", repo.Name, err)
		}
		name := model.CanonicalRepoKey(upstream.Name, upstream.Org(), repo.Name)
		if repo.ArchivedAt != nil {
			return fmt.Errorf("repository %s is archived; migrate or remove its registration before legacy execution removal", name)
		}
		capability, err := c.Catalog.ActiveWorkflowCapability(ctx, repo.ID)
		if err != nil {
			return fmt.Errorf("repository %s: read operator capability: %w", name, err)
		}
		sources, err := c.Git.RetirementSources(ctx, name)
		if err != nil {
			return fmt.Errorf("repository %s: inspect all branch declarations: %w", name, err)
		}
		for _, source := range sources {
			refs, err := retirement.CheckSource(source.Path, source.Body, capability != nil)
			if err == nil {
				err = c.checkFragments(ctx, refs)
			}
			if err != nil {
				return fmt.Errorf("repository %s ref %s commit %s file %s: %w", name, source.Ref, source.SHA, source.Path, err)
			}
		}
	}
	after, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(repositories, after) {
		return errors.New("registered repositories changed during retirement inspection; retry")
	}
	return c.checkObligations(ctx)
}

func (c LegacyRemovalCheck) checkFragments(ctx context.Context, refs []string) error {
	if len(refs) > argoworkflow.MaxFragments {
		return errors.New("retirement fragment inventory exceeds pipeline limit")
	}
	for _, ref := range refs {
		key, err := argoworkflow.ParseFragmentRef(ref)
		if err != nil {
			return err
		}
		if c.Fragments == nil {
			return fmt.Errorf("fragment %s: reader unavailable", ref)
		}
		fresh, err := c.Git.Ensure(ctx, key.Repo)
		if err != nil || fresh.Stale {
			return fmt.Errorf("fragment %s: fresh source unavailable", ref)
		}
		fragment, err := c.Fragments.Load(ctx, key, pipeline.TriggerCI, model.RefBranch, "branch")
		if err != nil {
			return fmt.Errorf("fragment %s: %w", ref, err)
		}
		nested, err := retirement.CheckSource(argoworkflow.FragmentFile, fragment.Source, false)
		if err != nil {
			return fmt.Errorf("pinned fragment %s commit %s: %w", ref, fragment.SHA, err)
		}
		if len(nested) > 0 {
			return fmt.Errorf("fragment %s has unsupported nested fragments", ref)
		}
	}
	return nil
}

func (c LegacyRemovalCheck) checkObligations(ctx context.Context) error {
	rows, err := c.Catalog.LegacyRetirementBlockers(ctx)
	if err != nil {
		return fmt.Errorf("read execution retirement obligations: %w", err)
	}
	if len(rows) > 0 {
		return fmt.Errorf("legacy removal refused while execution obligations remain: %s; keep the recovery server running and drain before retrying", strings.Join(rows, ", "))
	}
	return nil
}
