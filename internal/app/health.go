package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Version is the running server build, reported on the status view so the
	// dashboard can confirm which binary answered.
	Version string
	// Identity optionally reports the SSH public-key fingerprint used for
	// upstream Git authentication (never the private key).
	Identity func(context.Context) (string, error)
	// SecretStore optionally summarizes the configured OpenBao connection.
	// It carries configuration only — never a token or secret value.
	SecretStore *SecretStoreStatus
	// AuditChain optionally reports the local hash-chain head and the latest
	// externally anchored checkpoint.
	AuditChain func(context.Context) (AuditChainStatus, error)
}

// UpstreamStatus reports one configured upstream and its live probe result.
type UpstreamStatus struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	Probe   string `json:"probe"`
}

// SecretStoreStatus summarizes the operator-supplied OpenBao configuration.
// Address, mount, and role are deployment configuration (visible in the pod
// spec already); no credential material ever appears here.
type SecretStoreStatus struct {
	Configured bool   `json:"configured"`
	Address    string `json:"address,omitempty"`
	AuthMount  string `json:"auth_mount,omitempty"`
	Role       string `json:"role,omitempty"`
	Transport  string `json:"transport,omitempty"`
}

// AuditChainStatus reports the local audit hash-chain head and the latest
// external checkpoint. Detail carries the readiness diagnostic when the
// anchoring pipeline (TSA or Rekor witness) is unhealthy.
type AuditChainStatus struct {
	HeadID     int64  `json:"head_id"`
	HeadSHA256 string `json:"head_sha256,omitempty"`
	AnchorID   int64  `json:"anchor_id,omitempty"`
	TSAURL     string `json:"tsa_url,omitempty"`
	AnchoredAt string `json:"anchored_at,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type HealthStatus struct {
	Database     string             `json:"database"`
	Upstreams    int                `json:"upstreams"`
	Repositories int                `json:"repositories"`
	VCS          string             `json:"vcs"`
	Cluster      string             `json:"cluster"`
	Audit        string             `json:"audit"`
	Version      string             `json:"version,omitempty"`
	UpstreamInfo []UpstreamStatus   `json:"upstream_info,omitempty"`
	SSHIdentity  string             `json:"ssh_identity,omitempty"`
	SecretStore  *SecretStoreStatus `json:"secret_store,omitempty"`
	AuditChain   *AuditChainStatus  `json:"audit_chain,omitempty"`
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
	status := HealthStatus{Database: "unavailable", VCS: "unavailable", Cluster: "unavailable", Audit: "unavailable", Version: health.Version, SecretStore: health.SecretStore}
	if health.Store == nil {
		return status, nil
	}
	upstreams, upstreamErr := health.Store.ListUpstreams(ctx)
	repositories, repositoryErr := health.Store.ListRepositories(ctx)
	if upstreamErr == nil && repositoryErr == nil {
		status.Database = "ready"
		status.Upstreams = len(upstreams)
		status.Repositories = len(repositories)
		if len(upstreams) > 0 && health.VCS != nil {
			status.UpstreamInfo = health.probeVCS(ctx, upstreams)
			ready := true
			for _, upstream := range status.UpstreamInfo {
				if upstream.Probe != "ready" {
					ready = false
					break
				}
			}
			if ready {
				status.VCS = "ready"
			}
		}
	}
	if health.Cluster != nil && health.Cluster(ctx) == nil {
		status.Cluster = "ready"
	}
	auditErr := errors.New("audit probe is unavailable")
	if health.Audit != nil {
		auditErr = health.Audit(ctx)
		if auditErr == nil {
			status.Audit = "ready"
		}
	}
	if health.Identity != nil {
		if fingerprint, err := health.Identity(ctx); err == nil {
			status.SSHIdentity = fingerprint
		}
	}
	if health.AuditChain != nil {
		if chain, err := health.AuditChain(ctx); err == nil {
			if auditErr != nil {
				chain.Detail = boundedDetail(auditErr.Error())
			}
			status.AuditChain = &chain
		}
	}
	return status, nil
}

// probeVCS probes every configured upstream with a bounded deadline and
// reports each result individually, so one unreachable forge cannot hide the
// health of the others.
func (health Health) probeVCS(ctx context.Context, upstreams []model.Upstream) []UpstreamStatus {
	results := make([]UpstreamStatus, 0, len(upstreams))
	for _, upstream := range upstreams {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := health.VCS(probeCtx, upstream)
		cancel()
		probe := "ready"
		if err != nil {
			probe = boundedDetail(fmt.Errorf("probe upstream %s: %w", upstream.Name, err).Error())
		}
		results = append(results, UpstreamStatus{ID: upstream.ID, Name: upstream.Name, Kind: upstream.Kind, BaseURL: upstream.BaseURL, Probe: probe})
	}
	return results
}

// boundedDetail keeps diagnostic strings on the status view single-line and
// short enough for a dashboard cell.
func boundedDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	return detail
}
