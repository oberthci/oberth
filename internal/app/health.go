package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type HealthStore interface {
	ListUpstreams(context.Context) ([]model.Upstream, error)
	ListRepositories(context.Context) ([]model.Repository, error)
}

type ClusterProbe func(context.Context) error
type VCSProbe func(context.Context, model.Upstream) error

type Health struct {
	Store      HealthStore
	Configured ClusterProbe
	Cluster    ClusterProbe
	Audit      ClusterProbe
	VCS        VCSProbe
}

type HealthStatus struct {
	Database     string `json:"database"`
	Upstreams    int    `json:"upstreams"`
	Repositories int    `json:"repositories"`
	VCS          string `json:"vcs"`
	Cluster      string `json:"cluster"`
	Audit        string `json:"audit"`
}

// Ready gates the readiness probe. Like a vault before initialization, a
// freshly installed deployment stays alive but intentionally not ready until
// its first upstream is registered: the pod accepts in-pod administration
// (liveness passes, so no restarts) while the readiness probe keeps it out of
// Service endpoints. Once an upstream exists, readiness additionally requires
// its durable SSH credentials (when any non-local upstream exists), a
// responsive Kubernetes API, and externally anchored audit integrity.
func (health Health) Ready(ctx context.Context) error {
	if health.Store == nil || health.Configured == nil || health.Cluster == nil || health.Audit == nil || health.VCS == nil {
		return errors.New("app: health dependencies are unavailable")
	}
	upstreams, err := health.Store.ListUpstreams(ctx)
	if err != nil {
		return fmt.Errorf("app: database is unavailable: %w", err)
	}
	if len(upstreams) == 0 {
		return errors.New("app: no upstream is configured")
	}
	if requiresSSHIdentity(upstreams) {
		if err := health.Configured(ctx); err != nil {
			return fmt.Errorf("app: upstream configuration is unavailable: %w", err)
		}
	}
	if err := health.Cluster(ctx); err != nil {
		return fmt.Errorf("app: Kubernetes API is unavailable: %w", err)
	}
	if err := health.Audit(ctx); err != nil {
		return fmt.Errorf("app: audit integrity is unavailable: %w", err)
	}
	return nil
}

// requiresSSHIdentity returns true when at least one upstream uses an SSH-based
// transport (anything other than a local filesystem path).
func requiresSSHIdentity(upstreams []model.Upstream) bool {
	for _, upstream := range upstreams {
		if upstream.Kind != "local" {
			return true
		}
	}
	return false
}

func (health Health) Status(ctx context.Context) (any, error) {
	status := HealthStatus{Database: "unavailable", VCS: "unavailable", Cluster: "unavailable", Audit: "unavailable"}
	if health.Store == nil {
		return status, nil
	}
	upstreams, upstreamErr := health.Store.ListUpstreams(ctx)
	repositories, repositoryErr := health.Store.ListRepositories(ctx)
	if upstreamErr == nil && repositoryErr == nil {
		status.Database = "ready"
		status.Upstreams = len(upstreams)
		status.Repositories = len(repositories)
		if len(upstreams) > 0 && health.VCS != nil && health.probeVCS(ctx, upstreams) == nil {
			status.VCS = "ready"
		}
	}
	if health.Cluster != nil && health.Cluster(ctx) == nil {
		status.Cluster = "ready"
	}
	if health.Audit != nil && health.Audit(ctx) == nil {
		status.Audit = "ready"
	}
	return status, nil
}

func (health Health) probeVCS(ctx context.Context, upstreams []model.Upstream) error {
	for _, upstream := range upstreams {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := health.VCS(probeCtx, upstream)
		cancel()
		if err != nil {
			return fmt.Errorf("probe upstream %s: %w", upstream.Name, err)
		}
	}
	return nil
}
