// Package app contains the small adapters that compose Oberth's fresh FAB
// primitives into one server process.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const defaultCatalogTimeout = 5 * time.Second

type UpstreamCatalog interface {
	RepositoryByName(context.Context, string) (model.Repository, error)
	Upstream(context.Context, int64) (model.Upstream, error)
	ListUpstreams(context.Context) ([]model.Upstream, error)
}

// Upstreams maps client-owned repository names onto an operator-owned Forge
// base URL. An unknown repository can be discovered only when one upstream is
// configured; with multiple upstreams the mapping must already be durable.
type Upstreams struct {
	Catalog UpstreamCatalog
	Timeout time.Duration
}

func (upstreams Upstreams) Remote(repositoryName string) (string, error) {
	if upstreams.Catalog == nil {
		return "", errors.New("app: upstream catalog is required")
	}
	repositoryName, err := gitcache.NormalizeRepo(repositoryName)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), upstreams.timeout())
	defer cancel()
	upstream, err := upstreams.selectUpstream(ctx, repositoryName)
	if err != nil {
		return "", err
	}
	remote := joinUpstream(upstream.BaseURL, repositoryName)
	if err := gitcache.ValidateUpstream(remote); err != nil {
		return "", fmt.Errorf("app: resolve upstream %s: %w", upstream.Name, err)
	}
	return remote, nil
}

func (upstreams Upstreams) DiscoverRepository(ctx context.Context, repositoryName string) (model.RepositorySpec, error) {
	if upstreams.Catalog == nil {
		return model.RepositorySpec{}, errors.New("app: upstream catalog is required")
	}
	repositoryName, err := gitcache.NormalizeRepo(repositoryName)
	if err != nil {
		return model.RepositorySpec{}, err
	}
	upstream, err := upstreams.selectUpstream(ctx, repositoryName)
	if err != nil {
		return model.RepositorySpec{}, err
	}
	return model.RepositorySpec{Name: repositoryName, UpstreamID: upstream.ID}, nil
}

func (upstreams Upstreams) selectUpstream(ctx context.Context, repositoryName string) (model.Upstream, error) {
	repository, err := upstreams.Catalog.RepositoryByName(ctx, repositoryName)
	if err == nil {
		upstream, lookupErr := upstreams.Catalog.Upstream(ctx, repository.UpstreamID)
		if lookupErr != nil {
			return model.Upstream{}, fmt.Errorf("app: load repository upstream: %w", lookupErr)
		}
		return upstream, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Upstream{}, fmt.Errorf("app: look up repository mapping: %w", err)
	}
	values, err := upstreams.Catalog.ListUpstreams(ctx)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("app: list upstreams: %w", err)
	}
	if len(values) == 0 {
		return model.Upstream{}, errors.New("app: no upstream is configured")
	}
	if len(values) != 1 {
		return model.Upstream{}, fmt.Errorf("app: repository %s has no mapping and %d upstreams are configured", repositoryName, len(values))
	}
	return values[0], nil
}

func (upstreams Upstreams) timeout() time.Duration {
	if upstreams.Timeout <= 0 {
		return defaultCatalogTimeout
	}
	return upstreams.Timeout
}

func joinUpstream(baseURL, repositoryName string) string {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if filepath.IsAbs(baseURL) {
		return filepath.Join(baseURL, repositoryName+".git")
	}
	return baseURL + "/" + repositoryName + ".git"
}

func ValidateUpstreamBase(baseURL string) error {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.HasSuffix(strings.ToLower(baseURL), ".git") {
		return errors.New("app: upstream base must identify a repository namespace, not one repository")
	}
	if err := gitcache.ValidateUpstream(joinUpstream(baseURL, "oberth-probe")); err != nil {
		return fmt.Errorf("app: invalid upstream base: %w", err)
	}
	return nil
}

func UpstreamKind(baseURL string) (string, error) {
	if err := ValidateUpstreamBase(baseURL); err != nil {
		return "", err
	}
	if filepath.IsAbs(strings.TrimSpace(baseURL)) {
		return "local", nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("app: parse upstream base: %w", err)
	}
	return parsed.Scheme, nil
}
