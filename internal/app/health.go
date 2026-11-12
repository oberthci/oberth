package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type HealthStore interface {
	ListUpstreams(context.Context) ([]model.Upstream, error)
	ListRepositories(context.Context) ([]model.Repository, error)
}

type ClusterProbe func(context.Context) error
type VCSProbe func(context.Context, model.Upstream) error

type Health struct {
	// PipelineNetworkPolicyEnabled is the deployment configuration, not a live
	// enforcement measurement. Nil means configuration is unavailable.
	PipelineNetworkPolicyEnabled *bool
	ControllerProfile            func() *model.ControllerProfileStatus
	ExecutionTargetProfiles      func() map[string]*model.ControllerProfileStatus
	// ReadyGate checks process-local recovery state without network or store reads.
	ReadyGate ClusterProbe
	// Readiness is refreshed off the HTTP request path; nil retains direct probes.
	Readiness *ReadinessSnapshot
	// ReadinessUpstreams reserves a DB reader for the background sampler;
	// dashboard/mapping queries must not consume its connection. Nil uses Store.
	ReadinessUpstreams func(context.Context) ([]model.Upstream, error)
	// WriterReady reports completed hard write failures without touching storage.
	WriterReady func() error
	// Log records readiness transitions and the watcher's first retained deadline.
	Log          func(string, ...any)
	Storage      func(context.Context) (any, error)
	Store        HealthStore
	Configured   func(context.Context, []model.Upstream) error
	Cluster      ClusterProbe
	ClusterCache *ClusterSnapshot
	Audit        ClusterProbe
	VCS          VCSProbe
	// VCSCache is refreshed by WatchStatusProbes. Requests only read it.
	// A nil snapshot reports unavailable until configured and sampled.
	VCSCache *VCSSnapshot
	// Version is the running server build, reported on the status view so the
	// dashboard can confirm which binary answered.
	Version string
	// Identity optionally reports the SSH public-key fingerprint used for
	// upstream Git authentication (never the private key).
	Identity func(context.Context) (string, error)
	// SSHHostKey is this server's own SSH host public key (authorized-key
	// form), published on both status views so Git clients can pin it from
	// the CA-verified HTTPS API instead of trusting it on first use.
	SSHHostKey ssh.PublicKey
	// SecretStore optionally summarizes the configured OpenBao connection.
	// It carries configuration only — never a token or secret value.
	SecretStore *SecretStoreStatus
	// SecretStoreProbe optionally exercises the live secret-store trust
	// chain (SA token → Kubernetes-auth login → logout). When set,
	// WatchStatusProbes runs it on a TTL-cached schedule and includes the result
	// in SecretStoreStatus.Probe.
	SecretStoreProbe func(context.Context) error
	// SecretStoreCache is a process-shared TTL snapshot for the
	// secret-store login probe. Requests never initiate or wait for a refresh.
	SecretStoreCache *SecretStoreProbeSnapshot
	// SecretStoreSealed optionally checks whether the secret store is sealed
	// using the unauthenticated seal-status endpoint. Called only when the
	// login probe fails, to distinguish a sealed store from other failures.
	SecretStoreSealed func(context.Context) (bool, error)
	// AdmissionBlocker optionally reports the run blocking the admission
	// queue head (#1021).
	AdmissionBlocker func() *model.AdmissionBlocker
	// Admission optionally reports the whole admission gate (#1099).
	Admission       func() *model.AdmissionState
	DatabaseRetries func() uint64
	// SchemaVersion is the database schema version the running binary last
	// applied, reported on the status view and the version command so the
	// upgrade path can detect pending forward-only migrations.
	SchemaVersion int
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
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	BaseURL   string    `json:"base_url"`
	Probe     string    `json:"probe"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// SecretStoreStatus summarizes the operator-supplied OpenBao configuration.
// Address, mount, and role are deployment configuration (visible in the pod
// spec already); no credential material ever appears here.
type SecretStoreStatus struct {
	Configured bool      `json:"configured"`
	Address    string    `json:"address,omitempty"`
	AuthMount  string    `json:"auth_mount,omitempty"`
	Role       string    `json:"role,omitempty"`
	Transport  string    `json:"transport,omitempty"`
	Probe      string    `json:"probe,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitzero"`
	Sealed     bool      `json:"sealed,omitempty"`
	Remedy     string    `json:"remedy,omitempty"`
}

// AuditChainStatus reports the local audit hash-chain head and the latest
// external checkpoint. Anchored is false when no external witness or
// timestamp authority is configured (local-only mode). Detail carries the
// status diagnostic when the anchoring pipeline (TSA or Rekor witness) is
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
	QueueCap                *model.QueueCapStatus                     `json:"queue_cap,omitempty"`
	DatabaseRetries         uint64                                    `json:"database_retries_total,omitempty"`
	ControllerProfile       *model.ControllerProfileStatus            `json:"controller_profile,omitempty"`
	ExecutionTargetProfiles map[string]*model.ControllerProfileStatus `json:"execution_target_profiles,omitempty"`
	Storage                 any                                       `json:"storage,omitempty"`
	Database                string                                    `json:"database"`
	SchemaVersion           int                                       `json:"schema_version,omitempty"`
	Upstreams               int                                       `json:"upstreams"`
	Repositories            int                                       `json:"repositories"`
	VCS                     string                                    `json:"vcs"`
	Cluster                 string                                    `json:"cluster"`
	ClusterCheckedAt        time.Time                                 `json:"cluster_checked_at,omitzero"`
	ClusterDetail           string                                    `json:"cluster_detail"`
	Audit                   string                                    `json:"audit"`
	AuditMode               string                                    `json:"audit_mode,omitempty"`
	Version                 string                                    `json:"version,omitempty"`
	UpstreamInfo            []UpstreamStatus                          `json:"upstream_info,omitempty"`
	SSHIdentity             string                                    `json:"ssh_identity,omitempty"`
	// SSHHostKey and SSHHostKeyFingerprint describe the server's own SSH
	// endpoint (git push/clone), not an upstream identity.
	SSHHostKey            string              `json:"ssh_host_key,omitempty"`
	SSHHostKeyFingerprint string              `json:"ssh_host_key_fingerprint,omitempty"`
	SecretStore           *SecretStoreStatus  `json:"secret_store,omitempty"`
	AuditChain            *AuditChainStatus   `json:"audit_chain,omitempty"`
	LastBackup            *model.BackupStatus `json:"last_backup,omitempty"`
	// LastBackupWarning explains an omitted last_backup when the latest
	// backup record exists but cannot be read.
	LastBackupWarning string `json:"last_backup_warning,omitempty"`
	// AdminTokens is when the installation's admin tokens expire, with a
	// warning inside two weeks of the earliest. Unscoped status only.
	AdminTokens *model.AdminTokenExpiry `json:"admin_tokens,omitempty"`
	// AdmissionBlocker names the run at the head of the admission queue that
	// every later run waits behind, e.g. an XL run waiting for a full drain
	// (#1021). Unscoped status only.
	AdmissionBlocker *model.AdmissionBlocker `json:"admission_blocker,omitempty"`
	// Admission is the admission gate as data (#1099): every reservation,
	// including retained ones, and every concurrency group in use. A scoped
	// view keeps only its repositories' entries and omits the shared budget.
	Admission *model.AdmissionState `json:"admission,omitempty"`
}

// Ready gates the readiness probe. Like a vault before initialization, a
// freshly installed deployment stays alive but intentionally not ready until
// its first upstream is registered: the pod accepts in-pod administration
// (liveness passes, so no restarts) while the readiness probe keeps it out of
// Service endpoints. Readiness also requires a usable database and durable
// SSH credentials when any non-local upstream exists. Kubernetes availability
// and periodic audit health are diagnostics in Status, not Service membership:
// taking those dependencies down must not remove the cached Git/SSH endpoint.
// Audit authorization remains enforced by each operation's mutation gate.
func (health Health) Ready(ctx context.Context) error {
	var gateErr error
	if health.ReadyGate != nil {
		gateErr = health.ReadyGate(ctx)
	}
	if health.Readiness != nil {
		err := health.observeReadiness(gateErr)
		if gateErr != nil {
			return model.ReadinessFailure("startup_recovery", err)
		}
		return model.ReadinessFailure(publicReadinessReason(err), err)
	}
	if gateErr != nil {
		return model.ReadinessFailure("startup_recovery", gateErr)
	}
	err := health.probeReady(ctx)
	return model.ReadinessFailure(publicReadinessReason(err), err)
}

func (health Health) probeReady(ctx context.Context) error {
	_, err := health.probeReadySample(ctx)
	return err
}

// probeReadySample measures the database check, including connection-pool wait
// and query execution, separately from credential checks.
func (health Health) probeReadySample(ctx context.Context) (time.Duration, error) {
	if health.Store == nil || health.Configured == nil {
		return 0, errReadinessDependencies
	}
	if health.WriterReady != nil {
		if err := health.WriterReady(); err != nil {
			return 0, fmt.Errorf("%w: %w: %w", errReadinessDatabase, errReadinessWriter, err)
		}
	}
	started := time.Now()
	read := health.ReadinessUpstreams
	if read == nil {
		read = health.Store.ListUpstreams
	}
	upstreams, err := read(ctx)
	dbWait := time.Since(started)
	if err != nil {
		return dbWait, fmt.Errorf("%w: %w", errReadinessDatabase, err)
	}
	if len(upstreams) == 0 {
		return dbWait, errReadinessNoUpstream
	}
	if requiresSSHIdentity(upstreams) {
		if err := health.Configured(ctx, upstreams); err != nil {
			return dbWait, fmt.Errorf("%w: %w", errReadinessConfiguration, err)
		}
	}
	return dbWait, ctx.Err()
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

// pipelineNetworkPolicyDetail is informational and never gates readiness or admission.
func (health Health) pipelineNetworkPolicyDetail() string {
	if health.PipelineNetworkPolicyEnabled == nil {
		return "pipeline NetworkPolicy configuration is unavailable; CNI enforcement has not been measured"
	}
	if !*health.PipelineNetworkPolicyEnabled {
		return "WARNING: pipeline NetworkPolicy is configured disabled; Oberth does not configure pipeline egress isolation"
	}
	return "pipeline NetworkPolicy is configured enabled; CNI enforcement has not been measured"
}

func (health Health) Status(ctx context.Context) (any, error) {
	status := HealthStatus{Database: "unavailable", SchemaVersion: health.SchemaVersion, VCS: "unavailable", Cluster: "unavailable", ClusterDetail: health.pipelineNetworkPolicyDetail(), Audit: "unavailable", AuditMode: health.AuditMode, Version: health.Version, SecretStore: health.SecretStore}
	status.SSHHostKey, status.SSHHostKeyFingerprint = health.hostKey()
	if health.DatabaseRetries != nil {
		status.DatabaseRetries = health.DatabaseRetries()
	}
	if health.ControllerProfile != nil {
		status.ControllerProfile = health.ControllerProfile()
	}
	if health.ExecutionTargetProfiles != nil {
		status.ExecutionTargetProfiles = health.ExecutionTargetProfiles()
	}
	if health.Storage != nil {
		if storage, err := health.Storage(ctx); err == nil {
			status.Storage = storage
		}
	}
	if health.Store == nil {
		return status, nil
	}
	var queueCapErr error
	if reader, ok := health.Store.(interface {
		QueueCap(context.Context) (model.QueueCapStatus, error)
	}); ok {
		cap, err := reader.QueueCap(ctx)
		queueCapErr = err
		if err == nil {
			status.QueueCap = &cap
		}
	}
	upstreams, upstreamErr := health.Store.ListUpstreams(ctx)
	repositories, repositoryErr := health.Store.ListRepositories(ctx)
	if upstreamErr == nil && repositoryErr == nil {
		status.Database = "ready"
		status.Upstreams = len(upstreams)
		status.Repositories = len(repositories)
		if len(upstreams) > 0 && health.VCS != nil {
			status.UpstreamInfo = health.probeVCSForRepositories(upstreams, repositories)
			ready := true
			for _, upstream := range status.UpstreamInfo {
				if upstream.Probe != "ready" && upstream.Probe != "archived" {
					ready = false
					break
				}
			}
			if ready {
				status.VCS = "ready"
			}
		}
	}
	status.Cluster, status.ClusterCheckedAt = health.clusterStatus()
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
		probe, checkedAt := health.secretStoreStatus()
		// Set the probe result on a per-call copy so concurrent Status
		// callers never mutate the shared Health.SecretStore pointer.
		storeCopy := *health.SecretStore
		storeCopy.Probe = probe
		storeCopy.CheckedAt = checkedAt
		storeCopy.Sealed = false
		storeCopy.Remedy = ""
		if probe == "sealed" {
			storeCopy.Sealed = true
			storeCopy.Remedy = "sealed: run /oberth:unseal"
		}
		status.SecretStore = &storeCopy
	}
	if health.AdmissionBlocker != nil {
		status.AdmissionBlocker = health.AdmissionBlocker()
	}
	if reader, ok := health.Store.(interface {
		LastBackup(context.Context) (*model.BackupStatus, error)
	}); ok {
		backup, err := reader.LastBackup(ctx)
		switch {
		case err == nil:
			status.LastBackup = backup
		case errors.Is(err, model.ErrBackupRecordUnreadable):
			status.LastBackupWarning = boundedDetail(err.Error())
		default:
			status.Database = "unavailable"
		}
	}
	if reader, ok := health.Store.(interface {
		AdminTokenExpiry(context.Context) (*model.AdminTokenExpiry, error)
	}); ok {
		if expiry, err := reader.AdminTokenExpiry(ctx); err == nil {
			status.AdminTokens = expiry
		}
	}
	if health.Admission != nil {
		status.Admission = health.Admission()
	}
	if queueCapErr != nil {
		status.Database = "unavailable"
	}
	return status, nil
}

func (health Health) hostKey() (string, string) {
	if health.SSHHostKey == nil {
		return "", ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(health.SSHHostKey))), ssh.FingerprintSHA256(health.SSHHostKey)
}

// ScopedStatus is the status an upstream-scoped uplink sees (#073): only its
// upstreams and their repositories are counted and shown. The audit chain
// and the secret store describe the whole installation, so they are left out.
func (health Health) ScopedStatus(ctx context.Context, actor api.Actor) (any, error) {
	status := HealthStatus{Database: "unavailable", SchemaVersion: health.SchemaVersion, VCS: "unavailable", Cluster: "unavailable", ClusterDetail: health.pipelineNetworkPolicyDetail(), Audit: "unavailable", AuditMode: health.AuditMode, Version: health.Version}
	status.SSHHostKey, status.SSHHostKeyFingerprint = health.hostKey()
	if health.Store == nil {
		return status, nil
	}
	upstreams, upstreamErr := health.Store.ListUpstreams(ctx)
	repositories, repositoryErr := health.Store.ListRepositories(ctx)
	if upstreamErr == nil && repositoryErr == nil {
		status.Database = "ready"
		visible := map[int64]bool{}
		for _, upstream := range upstreams {
			if actor.SeesUpstream(upstream.Name) {
				visible[upstream.ID] = true
			}
		}
		status.Upstreams = len(visible)
		for _, repository := range repositories {
			if visible[repository.UpstreamID] {
				status.Repositories++
			}
		}
		if len(visible) > 0 && health.VCS != nil {
			// Read the full shared snapshot, then project only visible upstreams.
			ready := true
			for _, upstream := range health.probeVCSForRepositories(upstreams, repositories) {
				if !visible[upstream.ID] {
					continue
				}
				status.UpstreamInfo = append(status.UpstreamInfo, upstream)
				if upstream.Probe != "ready" && upstream.Probe != "archived" {
					ready = false
				}
			}
			if ready {
				status.VCS = "ready"
			}
		}
	}
	status.Cluster, status.ClusterCheckedAt = health.clusterStatus()
	if health.Audit != nil && health.Audit(ctx) == nil {
		status.Audit = "ready"
	}
	if health.Identity != nil {
		if fingerprint, err := health.Identity(ctx); err == nil {
			status.SSHIdentity = fingerprint
		}
	}
	if health.Admission != nil && upstreamErr == nil && repositoryErr == nil {
		status.Admission = scopedAdmission(health.Admission(), upstreams, repositories, actor)
	}
	return status, nil
}

// scopedAdmission keeps only the reservations and groups of repositories the
// actor's upstreams own, and withholds the shared budget and its use (#073):
// a full pool of invisible work must not show.
func scopedAdmission(state *model.AdmissionState, upstreams []model.Upstream, repositories []model.Repository, actor api.Actor) *model.AdmissionState {
	if state == nil {
		return nil
	}
	visibleUpstreams := map[int64]bool{}
	for _, upstream := range upstreams {
		if actor.SeesUpstream(upstream.Name) {
			visibleUpstreams[upstream.ID] = true
		}
	}
	visible := map[int64]bool{}
	for _, repository := range repositories {
		if visibleUpstreams[repository.UpstreamID] {
			visible[repository.ID] = true
		}
	}
	scoped := &model.AdmissionState{Reservations: []model.AdmissionReservation{}}
	for _, reservation := range state.Reservations {
		if visible[reservation.RepoID] {
			reservation.Position = 0
			reservation.Pressure = reservation.Pressure.Public()
			scoped.Reservations = append(scoped.Reservations, reservation)
		}
	}
	return scoped
}

const (
	// vcsProbeTTL is the minimum interval between full upstream probe cycles.
	// Background sampling interval; readers never refresh the snapshot.
	vcsProbeTTL = 10 * time.Second
	// vcsProbeTimeout is the per-upstream probe deadline.
	vcsProbeTimeout = 5 * time.Second
	// vcsProbeAggregate is the aggregate deadline across all concurrent probes.
	vcsProbeAggregate = 15 * time.Second
)

// VCSSnapshot retains the last completed background upstream sample.
// Readers return it immediately while a refresh is in progress.
type VCSSnapshot struct {
	key      string
	mu       sync.Mutex
	results  []UpstreamStatus
	inflight chan struct{} // non-nil while a refresh is in progress
}

// probeVCS refreshes the background snapshot, coalescing overlapping samples.
// The watcher owns the sampling interval; request readers use vcsStatus.
func (health Health) probeVCS(ctx context.Context, upstreams []model.Upstream) []UpstreamStatus {
	key := upstreamProbeKey(upstreams)
	cache := health.VCSCache
	if cache == nil {
		// No shared cache (test or single-caller path): probe directly.
		return health.probeVCSConcurrent(ctx, upstreams)
	}
	cache.mu.Lock()
	if cache.inflight != nil {
		wait := cache.inflight
		cache.mu.Unlock()
		select {
		case <-wait:
			return health.probeVCS(ctx, upstreams)
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
	cache.key = key
	cache.inflight = nil
	cache.mu.Unlock()
	close(done)
	return results
}

// probeVCSConcurrent bounds background SSH concurrency and the aggregate deadline.
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
	jobs := make(chan int, len(upstreams))
	for i := range upstreams {
		jobs <- i
	}
	close(jobs)
	for range min(4, len(upstreams)) {
		wg.Go(func() {
			for index := range jobs {
				u := upstreams[index]
				probeCtx, cancel := context.WithTimeout(aggCtx, vcsProbeTimeout)
				err := probeCtx.Err()
				if err == nil {
					err = health.VCS(probeCtx, u)
				}
				cancel()
				probe := "ready"
				if err != nil {
					probe = boundedDetail(fmt.Errorf("probe upstream %s: %w", u.Name, err).Error())
				}
				ch <- indexedResult{index: index, status: UpstreamStatus{ID: u.ID, Name: u.Name, Kind: u.Kind, BaseURL: u.BaseURL, Probe: probe, CheckedAt: time.Now().UTC()}}
			}
		})
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
	// Background sampling interval; readers never refresh the snapshot.
	secretStoreProbeTTL = 30 * time.Second
	// secretStoreProbeTimeout bounds a single probe's context.
	secretStoreProbeTimeout = 10 * time.Second
)

// SecretStoreProbeSnapshot retains the last completed login sample and its
// completion time. Requests read it without waiting for background refreshes.
type SecretStoreProbeSnapshot struct {
	mu       sync.Mutex
	result   string
	age      time.Time
	inflight chan struct{} // non-nil while a probe is in progress
}

// RefreshSecretStore samples the login/seal state and retains its completion
// time. Overlapping refreshes share one probe; the watcher owns the interval.
func (health Health) RefreshSecretStore(ctx context.Context) string {
	if health.SecretStoreProbe == nil || health.SecretStore == nil || !health.SecretStore.Configured {
		return ""
	}
	cache := health.SecretStoreCache
	if cache == nil {
		// No shared cache (test or single-caller path): probe directly.
		probeCtx, cancel := context.WithTimeout(ctx, secretStoreProbeTimeout)
		defer cancel()
		err := health.SecretStoreProbe(probeCtx)
		return health.classifyProbeResult(probeCtx, err)
	}
	cache.mu.Lock()
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
	cache.age = time.Now().UTC()
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

// An upstream with only archived repositories needs no live SSH probe. Empty
// upstreams still probe, so enrollment diagnostics retain their meaning.
func (health Health) probeVCSForRepositories(upstreams []model.Upstream, repositories []model.Repository) []UpstreamStatus {
	seen, active := map[int64]bool{}, map[int64]bool{}
	for _, repo := range repositories {
		seen[repo.UpstreamID] = true
		if repo.ArchivedAt == nil {
			active[repo.UpstreamID] = true
		}
	}
	probe := activeProbeUpstreams(upstreams, repositories)
	results := health.vcsStatus(probe)
	byID := map[int64]UpstreamStatus{}
	for _, result := range results {
		byID[result.ID] = result
	}
	all := make([]UpstreamStatus, 0, len(upstreams))
	for _, upstream := range upstreams {
		state := "unavailable"
		if seen[upstream.ID] && !active[upstream.ID] {
			state = "archived"
		} else if result, ok := byID[upstream.ID]; ok {
			all = append(all, result)
			continue
		}
		all = append(all, UpstreamStatus{ID: upstream.ID, Name: upstream.Name, Kind: upstream.Kind, BaseURL: upstream.BaseURL, Probe: state})
	}
	return all
}

// activeProbeUpstreams is shared by sampling and presentation so archived
// repositories never cause transport activity.
func activeProbeUpstreams(upstreams []model.Upstream, repositories []model.Repository) []model.Upstream {
	seen, active := map[int64]bool{}, map[int64]bool{}
	for _, repo := range repositories {
		seen[repo.UpstreamID] = true
		if repo.ArchivedAt == nil {
			active[repo.UpstreamID] = true
		}
	}
	result := make([]model.Upstream, 0, len(upstreams))
	for _, up := range upstreams {
		if !seen[up.ID] || active[up.ID] {
			result = append(result, up)
		}
	}
	return result
}
