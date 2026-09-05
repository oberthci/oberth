package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	// VCSCache is a process-shared TTL snapshot. When non-nil, concurrent
	// dashboard viewers share one refresh cycle; nil probes fresh every time.
	VCSCache *VCSSnapshot
	// Version is the running server build, reported on the status view so the
	// dashboard can confirm which binary answered.
	Version string
	// Identity optionally reports the SSH public-key fingerprint used for
	// upstream Git authentication (never the private key).
	Identity func(context.Context) (string, error)
	// SecretStore optionally summarizes the configured OpenBao connection.
	// It carries configuration only — never a token or secret value.
	SecretStore *SecretStoreStatus
	// SecretStoreProbe optionally exercises the live secret-store trust
	// chain (SA token → Kubernetes-auth login → logout). When set,
	// Status() runs it on a TTL-cached schedule and includes the result
	// in SecretStoreStatus.Probe.
	SecretStoreProbe func(context.Context) error
	// SecretStoreCache is a process-shared TTL snapshot for the
	// secret-store login probe. When non-nil, concurrent dashboard
	// viewers share one probe cycle; nil probes fresh every time.
	SecretStoreCache *SecretStoreProbeSnapshot
	// SecretStoreSealed optionally checks whether the secret store is sealed
	// using the unauthenticated seal-status endpoint. Called only when the
	// login probe fails, to distinguish a sealed store from other failures.
	SecretStoreSealed func(context.Context) (bool, error)
	// AuditMode reports the configured audit integrity mode: "anchored" when
	// an external witness or timestamp authority is configured, "local" when
	// only the SQLite hash chain runs.
	AuditMode string
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
	Probe      string `json:"probe,omitempty"`
	Sealed     bool   `json:"sealed,omitempty"`
}

// AuditChainStatus reports the local audit hash-chain head and the latest
// external checkpoint. Anchored is false when no external witness or
// timestamp authority is configured (local-only mode). Detail carries the
// readiness diagnostic when the anchoring pipeline (TSA or Rekor witness) is
// unhealthy.
type AuditChainStatus struct {
	HeadID     int64  `json:"head_id"`
	HeadSHA256 string `json:"head_sha256,omitempty"`
	Anchored   bool   `json:"anchored"`
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
	AuditMode    string             `json:"audit_mode,omitempty"`
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
	status := HealthStatus{Database: "unavailable", VCS: "unavailable", Cluster: "unavailable", Audit: "unavailable", AuditMode: health.AuditMode, Version: health.Version, SecretStore: health.SecretStore}
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
	if health.SecretStoreProbe != nil && health.SecretStore != nil && health.SecretStore.Configured {
		probe := health.probeSecretStore(ctx)
		// Set the probe result on a per-call copy so concurrent Status
		// callers never mutate the shared Health.SecretStore pointer.
		storeCopy := *health.SecretStore
		storeCopy.Probe = probe
		if probe == "sealed" {
			storeCopy.Sealed = true
		}
		status.SecretStore = &storeCopy
	}
	return status, nil
}

const (
	// vcsProbeTTL is the minimum interval between full upstream probe cycles.
	// Concurrent dashboard viewers share the cached result.
	vcsProbeTTL = 10 * time.Second
	// vcsProbeTimeout is the per-upstream probe deadline.
	vcsProbeTimeout = 5 * time.Second
	// vcsProbeAggregate is the aggregate deadline across all concurrent probes.
	vcsProbeAggregate = 15 * time.Second
)

// VCSSnapshot is a process-shared, TTL-bounded cache of upstream probe
// results. Concurrent viewers read the latest result and its age; at most one
// goroutine refreshes at a time. Create one per process and assign it to
// Health.VCSCache; a nil cache probes every time (tests, single-call paths).
type VCSSnapshot struct {
	mu       sync.Mutex
	results  []UpstreamStatus
	age      time.Time
	inflight chan struct{} // non-nil while a refresh is in progress
}

// probeVCS returns the cached probe result when it is fresh enough, or
// triggers a concurrent refresh and waits for it. Concurrent viewers coalesce
// into the same refresh, never duplicate SSH sessions.
func (health Health) probeVCS(ctx context.Context, upstreams []model.Upstream) []UpstreamStatus {
	cache := health.VCSCache
	if cache == nil {
		// No shared cache (test or single-caller path): probe directly.
		return health.probeVCSConcurrent(ctx, upstreams)
	}
	cache.mu.Lock()
	if time.Since(cache.age) < vcsProbeTTL && cache.results != nil {
		results := cache.results
		cache.mu.Unlock()
		return results
	}
	if cache.inflight != nil {
		wait := cache.inflight
		cache.mu.Unlock()
		select {
		case <-wait:
			cache.mu.Lock()
			results := cache.results
			cache.mu.Unlock()
			return results
		case <-ctx.Done():
			cache.mu.Lock()
			results := cache.results
			cache.mu.Unlock()
			return results
		}
	}
	done := make(chan struct{})
	cache.inflight = done
	cache.mu.Unlock()

	results := health.probeVCSConcurrent(ctx, upstreams)

	cache.mu.Lock()
	cache.results = results
	cache.age = time.Now()
	cache.inflight = nil
	cache.mu.Unlock()
	close(done)
	return results
}

// probeVCSConcurrent probes every upstream concurrently with a bounded worker
// pool and one aggregate deadline, so one unreachable forge cannot block the
// response while the others return fast.
func (health Health) probeVCSConcurrent(ctx context.Context, upstreams []model.Upstream) []UpstreamStatus {
	aggCtx, aggCancel := context.WithTimeout(ctx, vcsProbeAggregate)
	defer aggCancel()

	type indexedResult struct {
		index  int
		status UpstreamStatus
	}
	results := make([]UpstreamStatus, len(upstreams))
	ch := make(chan indexedResult, len(upstreams))
	var wg sync.WaitGroup
	for i, upstream := range upstreams {
		wg.Add(1)
		go func(index int, u model.Upstream) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(aggCtx, vcsProbeTimeout)
			err := health.VCS(probeCtx, u)
			cancel()
			probe := "ready"
			if err != nil {
				probe = boundedDetail(fmt.Errorf("probe upstream %s: %w", u.Name, err).Error())
			}
			ch <- indexedResult{index: index, status: UpstreamStatus{ID: u.ID, Name: u.Name, Kind: u.Kind, BaseURL: u.BaseURL, Probe: probe}}
		}(i, upstream)
	}
	go func() { wg.Wait(); close(ch) }()
	for result := range ch {
		results[result.index] = result.status
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

const (
	// secretStoreProbeTTL is the minimum interval between login probes.
	// Concurrent dashboard viewers share the cached result.
	secretStoreProbeTTL = 30 * time.Second
	// secretStoreProbeTimeout bounds a single probe's context.
	secretStoreProbeTimeout = 10 * time.Second
)

// SecretStoreProbeSnapshot is a process-shared, TTL-bounded cache of the
// secret-store login probe result. Concurrent viewers read the latest result
// and its age; at most one goroutine probes at a time. Create one per process
// and assign it to Health.SecretStoreCache; a nil cache probes every time
// (tests, single-call paths).
type SecretStoreProbeSnapshot struct {
	mu       sync.Mutex
	result   string
	age      time.Time
	inflight chan struct{} // non-nil while a probe is in progress
}

// probeSecretStore returns the cached probe result when it is fresh enough,
// or triggers a single probe and waits for it. Concurrent viewers coalesce
// into the same probe, never duplicate OpenBao logins.
func (health Health) probeSecretStore(ctx context.Context) string {
	cache := health.SecretStoreCache
	if cache == nil {
		// No shared cache (test or single-caller path): probe directly.
		probeCtx, cancel := context.WithTimeout(ctx, secretStoreProbeTimeout)
		defer cancel()
		err := health.SecretStoreProbe(probeCtx)
		return health.classifyProbeResult(probeCtx, err)
	}
	cache.mu.Lock()
	if time.Since(cache.age) < secretStoreProbeTTL && cache.result != "" {
		result := cache.result
		cache.mu.Unlock()
		return result
	}
	if cache.inflight != nil {
		wait := cache.inflight
		cache.mu.Unlock()
		select {
		case <-wait:
			cache.mu.Lock()
			result := cache.result
			cache.mu.Unlock()
			return result
		case <-ctx.Done():
			cache.mu.Lock()
			result := cache.result
			cache.mu.Unlock()
			if result != "" {
				return result
			}
			return "unavailable"
		}
	}
	done := make(chan struct{})
	cache.inflight = done
	cache.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, secretStoreProbeTimeout)
	defer cancel()
	err := health.SecretStoreProbe(probeCtx)
	result := health.classifyProbeResult(probeCtx, err)

	cache.mu.Lock()
	cache.result = result
	cache.age = time.Now()
	cache.inflight = nil
	cache.mu.Unlock()
	close(done)
	return result
}

// classifyProbeResult distinguishes a sealed store from other login failures.
// When the login probe fails and a seal-status callback is available, a
// confirmed sealed state returns the machine-contract string "sealed".
// If the seal-status check itself errors, the original login error detail is
// preserved — we cannot distinguish sealed from unreachable in that case.
func (health Health) classifyProbeResult(ctx context.Context, loginErr error) string {
	if loginErr == nil {
		return "ready"
	}
	if health.SecretStoreSealed != nil {
		if sealed, err := health.SecretStoreSealed(ctx); err == nil && sealed {
			return "sealed"
		}
	}
	return boundedDetail(loginErr.Error())
}

// RefreshSecretStore forces a fresh secret-store probe, updating the cached
// snapshot that concurrent Status() callers read. Returns the probe result
// string ("ready", "sealed", or the bounded error detail). With a 60s
// periodic tick and 30s TTL, the cache is always stale when the ticker fires,
// so routing through probeSecretStore naturally refreshes.
func (health Health) RefreshSecretStore(ctx context.Context) string {
	if health.SecretStoreProbe == nil || health.SecretStore == nil || !health.SecretStore.Configured {
		return ""
	}
	return health.probeSecretStore(ctx)
}

// ProbeClass classifies a secret-store probe result string into one of three
// states for transition detection: "ready" for a healthy or empty probe,
// "sealed" for a sealed store, and "failing" for any other error.
func ProbeClass(result string) string {
	switch result {
	case "ready", "":
		return "ready"
	case "sealed":
		return "sealed"
	default:
		return "failing"
	}
}

// SecretStoreObserver tracks probe result transitions and logs only on state
// changes. Steady-state results produce no output. The Address field is
// included in log messages for operational context. The Log function is
// typically logger.Printf.
type SecretStoreObserver struct {
	previous string
	Log      func(string, ...any)
	Address  string
}

// Observe classifies the probe result and logs if the state class changed
// since the last observation. Steady-state (no transition) is silent.
func (o *SecretStoreObserver) Observe(result string) {
	current := ProbeClass(result)
	previous := o.previous
	if previous == "" {
		previous = "ready" // zero value: assume healthy at startup
	}
	if current == previous {
		return
	}
	switch current {
	case "sealed":
		o.Log("WARNING: secret store SEALED at %s \u2014 credentialed releases and access grants will fail until unsealed (branch CI unaffected)", o.Address)
	case "failing":
		o.Log("WARNING: secret store probe failing at %s: %s", o.Address, result)
	case "ready":
		o.Log("secret store probe recovered at %s", o.Address)
	}
	o.previous = current
}
