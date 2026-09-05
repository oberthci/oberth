package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type fakeHealthStore struct {
	upstreams    []model.Upstream
	repositories []model.Repository
	err          error
}

func (store fakeHealthStore) ListUpstreams(context.Context) ([]model.Upstream, error) {
	return store.upstreams, store.err
}

func (store fakeHealthStore) ListRepositories(context.Context) ([]model.Repository, error) {
	return store.repositories, store.err
}

func TestHealthRequiresConfiguredUpstreamAndCluster(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{}, Configured: func(context.Context) error { return nil },
		Cluster: func(context.Context) error { return nil }, VCS: func(context.Context, model.Upstream) error { return nil },
		Audit: func(context.Context) error { return nil },
	}
	// Vault-before-init semantics: with zero upstreams the pod must stay alive
	// but not ready, even when every other dependency is green.
	if err := health.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "no upstream is configured") {
		t.Fatalf("fresh install readiness = %v, want not-ready until an upstream is registered", err)
	}
	health.Store = fakeHealthStore{err: errors.New("database locked")}
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with an unavailable database")
	}
	health.Store = fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}}
	health.Configured = func(context.Context) error { return errors.New("credentials missing") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed without durable upstream credentials")
	}
	health.Configured = func(context.Context) error { return nil }
	health.VCS = func(context.Context, model.Upstream) error { return errors.New("VCS down") }
	if err := health.Ready(context.Background()); err != nil {
		t.Fatalf("transient VCS outage removed cached service from readiness: %v", err)
	}
	health.VCS = func(context.Context, model.Upstream) error { return nil }
	health.Cluster = func(context.Context) error { return errors.New("cluster down") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with an unavailable cluster")
	}
	health.Cluster = func(context.Context) error { return nil }
	health.Audit = func(context.Context) error { return errors.New("anchor stale") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with stale audit anchoring")
	}
	health.Audit = func(context.Context) error { return nil }
	if err := health.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSkipsSSHCheckForLocalOnlyUpstreams(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Kind: "local"}}},
		Configured: func(context.Context) error { return errors.New("SSH identity missing") },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
	}
	// A local-only upstream does not need SSH credentials.
	if err := health.Ready(context.Background()); err != nil {
		t.Fatalf("local-only readiness failed: %v", err)
	}
	// Adding a non-local upstream should require SSH credentials.
	health.Store = fakeHealthStore{upstreams: []model.Upstream{
		{ID: 1, Kind: "local"},
		{ID: 2, Kind: "ssh"},
	}}
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed without SSH credentials despite non-local upstream")
	}
}

func TestHealthStatusIsUsefulDuringOutage(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}, repositories: []model.Repository{{ID: 1}, {ID: 2}}},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return errors.New("cluster down") },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Database != "ready" || status.VCS != "ready" || status.Cluster != "unavailable" || status.Audit != "ready" || status.Upstreams != 1 || status.Repositories != 2 {
		t.Fatalf("status = %+v", status)
	}
}

func TestHealthStatusReportsDashboardDetail(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams: []model.Upstream{
				{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"},
				{ID: 2, Name: "mirror", Kind: "github", BaseURL: "git@github.com:oberthci"},
			},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return errors.New("audit anchor: latest checkpoint is stale by 3m") },
		VCS: func(_ context.Context, upstream model.Upstream) error {
			if upstream.Name == "mirror" {
				return errors.New("connection refused")
			}
			return nil
		},
		Version:     "v0.10.52-test",
		Identity:    func(context.Context) (string, error) { return "SHA256:abcdef", nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao.internal:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		AuditMode:   "anchored",
		AuditChain: func(context.Context) (AuditChainStatus, error) {
			return AuditChainStatus{HeadID: 41, HeadSHA256: "aa11", Anchored: true, AnchorID: 7, TSAURL: "https://tsa.example", AnchoredAt: "2026-08-08T10:00:00Z"}, nil
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Version != "v0.10.52-test" || status.SSHIdentity != "SHA256:abcdef" {
		t.Fatalf("status identity = %+v", status)
	}
	// One unreachable upstream keeps overall VCS unavailable while each
	// upstream still reports its own probe result.
	if status.VCS != "unavailable" || len(status.UpstreamInfo) != 2 {
		t.Fatalf("upstream detail = %+v", status)
	}
	if status.UpstreamInfo[0].Probe != "ready" || !strings.Contains(status.UpstreamInfo[1].Probe, "connection refused") {
		t.Fatalf("per-upstream probes = %+v", status.UpstreamInfo)
	}
	if status.SecretStore == nil || !status.SecretStore.Configured || status.SecretStore.Address != "https://bao.internal:8200" {
		t.Fatalf("secret store detail = %+v", status.SecretStore)
	}
	// The stale audit anchor keeps audit unavailable and surfaces its
	// diagnostic on the chain detail.
	if status.Audit != "unavailable" || status.AuditChain == nil || status.AuditChain.HeadID != 41 || !strings.Contains(status.AuditChain.Detail, "stale") {
		t.Fatalf("audit chain detail = %+v", status.AuditChain)
	}
	if status.AuditMode != "anchored" || !status.AuditChain.Anchored {
		t.Fatalf("audit mode = %+v", status)
	}
}

// TestHealthStatusReportsLocalAuditMode covers the default configuration with
// no external anchoring: audit reports ready from the verified local hash
// chain alone, the mode reads "local", and the chain carries no checkpoint.
func TestHealthStatusReportsLocalAuditMode(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
		AuditMode:  "local",
		AuditChain: func(context.Context) (AuditChainStatus, error) {
			return AuditChainStatus{HeadID: 3, HeadSHA256: "bb22", Anchored: false}, nil
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Audit != "ready" || status.AuditMode != "local" {
		t.Fatalf("local audit status = %+v", status)
	}
	if status.AuditChain == nil || status.AuditChain.Anchored || status.AuditChain.HeadID != 3 ||
		status.AuditChain.AnchorID != 0 || status.AuditChain.Detail != "" {
		t.Fatalf("local audit chain = %+v", status.AuditChain)
	}
}

func TestHealthStatusIncludesSecretStoreProbeReady(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:       func(context.Context) error { return nil },
		Cluster:          func(context.Context) error { return nil },
		Audit:            func(context.Context) error { return nil },
		VCS:              func(context.Context, model.Upstream) error { return nil },
		SecretStore:      &SecretStoreStatus{Configured: true, Address: "https://bao.internal:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error { return nil },
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || status.SecretStore.Probe != "ready" {
		t.Fatalf("secret store probe = %+v, want ready", status.SecretStore)
	}
	// The original Health.SecretStore pointer must not be mutated.
	if health.SecretStore.Probe != "" {
		t.Fatalf("shared Health.SecretStore was mutated: Probe = %q", health.SecretStore.Probe)
	}
}

func TestHealthStatusIncludesSecretStoreProbeFailure(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao.internal:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error {
			return errors.New("secret store Kubernetes auth login failed: HTTP 503 Service Unavailable")
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || !strings.Contains(status.SecretStore.Probe, "503") {
		t.Fatalf("secret store probe = %+v, want 503 error", status.SecretStore)
	}
}

func TestHealthStatusOmitsProbeWhenUnconfigured(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: false},
		// Probe is set but store is not configured — the probe must not run.
		SecretStoreProbe: func(context.Context) error {
			t.Fatal("probe must not run when the store is not configured")
			return nil
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || status.SecretStore.Probe != "" {
		t.Fatalf("unconfigured store should have no probe: %+v", status.SecretStore)
	}
}

func TestHealthStatusConcurrentSecretStoreProbeNoDataRace(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:       func(context.Context) error { return nil },
		Cluster:          func(context.Context) error { return nil },
		Audit:            func(context.Context) error { return nil },
		VCS:              func(context.Context, model.Upstream) error { return nil },
		SecretStore:      &SecretStoreStatus{Configured: true, Address: "https://bao.internal:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error { return nil },
		SecretStoreCache: &SecretStoreProbeSnapshot{},
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				value, err := health.Status(context.Background())
				if err != nil {
					t.Errorf("Status() error = %v", err)
					return
				}
				status := value.(HealthStatus)
				if status.SecretStore == nil || status.SecretStore.Probe != "ready" {
					t.Errorf("concurrent probe = %+v", status.SecretStore)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestHealthProbeReportsSealed(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error {
			return errors.New("secret store Kubernetes auth login failed: HTTP 503 Service Unavailable")
		},
		SecretStoreSealed: func(context.Context) (bool, error) {
			return true, nil
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || status.SecretStore.Probe != "sealed" {
		t.Fatalf("secret store probe = %+v, want sealed", status.SecretStore)
	}
	if !status.SecretStore.Sealed {
		t.Fatal("SecretStore.Sealed = false, want true")
	}
}

func TestHealthProbeReportsLoginDetailWhenSealStatusErrors(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error {
			return errors.New("connection refused")
		},
		SecretStoreSealed: func(context.Context) (bool, error) {
			return false, errors.New("seal-status also unreachable")
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || status.SecretStore.Probe == "sealed" {
		t.Fatalf("probe should show login error, not sealed: %+v", status.SecretStore)
	}
	if !strings.Contains(status.SecretStore.Probe, "connection refused") {
		t.Fatalf("probe = %q, want login error detail", status.SecretStore.Probe)
	}
	if status.SecretStore.Sealed {
		t.Fatal("SecretStore.Sealed = true, want false when seal-status errored")
	}
}

func TestHealthProbeReadyIgnoresSealedCallback(t *testing.T) {
	t.Parallel()
	sealCalled := false
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error {
			return nil // login succeeds
		},
		SecretStoreSealed: func(context.Context) (bool, error) {
			sealCalled = true
			return true, nil // would report sealed if consulted
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore == nil || status.SecretStore.Probe != "ready" {
		t.Fatalf("probe = %+v, want ready", status.SecretStore)
	}
	if sealCalled {
		t.Fatal("seal-status callback must not be consulted when login succeeds")
	}
}

func TestRefreshSecretStoreUpdatesCachedSnapshot(t *testing.T) {
	t.Parallel()
	var probeCalls sync.WaitGroup
	var probeCount int64
	health := Health{
		Store: fakeHealthStore{
			upstreams:    []model.Upstream{{ID: 1, Name: "codeberg", Kind: "forgejo", BaseURL: "git@codeberg.org:cloudtaser"}},
			repositories: []model.Repository{{ID: 1}},
		},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao:8200", AuthMount: "kubernetes", Role: "oberth", Transport: "https"},
		SecretStoreProbe: func(context.Context) error {
			probeCount++
			probeCalls.Done()
			return nil
		},
		SecretStoreCache: &SecretStoreProbeSnapshot{},
	}
	// RefreshSecretStore forces a probe, writing the result to the cache.
	probeCalls.Add(1)
	result := health.RefreshSecretStore(context.Background())
	probeCalls.Wait()
	if result != "ready" || probeCount != 1 {
		t.Fatalf("refresh = %q, probeCount = %d", result, probeCount)
	}
	// Status() within TTL reads the cached result without re-probing.
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SecretStore.Probe != "ready" || probeCount != 1 {
		t.Fatalf("cached viewer saw %q, probeCount = %d (want 1, no re-probe)", status.SecretStore.Probe, probeCount)
	}
}

func TestProbeClassClassifiesStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"ready", "ready"},
		{"", "ready"},
		{"sealed", "sealed"},
		{"connection refused", "failing"},
		{"secret store Kubernetes auth login failed: HTTP 503", "failing"},
	}
	for _, test := range tests {
		if got := ProbeClass(test.input); got != test.want {
			t.Errorf("ProbeClass(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestSecretStoreObserverLogsOnlyTransitions(t *testing.T) {
	t.Parallel()
	var messages []string
	observer := &SecretStoreObserver{
		Log: func(format string, args ...any) {
			messages = append(messages, fmt.Sprintf(format, args...))
		},
		Address: "https://bao:8200",
	}
	// Sequence: ready -> sealed -> sealed (steady) -> ready (recovery)
	observer.Observe("ready")  // initial state, no log (previous is "" which classifies as "ready")
	observer.Observe("sealed") // transition -> WARNING
	observer.Observe("sealed") // steady state, no log
	observer.Observe("ready")  // recovery -> log
	if len(messages) != 2 {
		t.Fatalf("expected 2 log messages, got %d: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0], "WARNING") || !strings.Contains(messages[0], "SEALED") {
		t.Fatalf("first message not a sealed warning: %q", messages[0])
	}
	if !strings.Contains(messages[1], "recovered") {
		t.Fatalf("second message not a recovery: %q", messages[1])
	}
}

func TestSecretStoreObserverLogsFailingTransition(t *testing.T) {
	t.Parallel()
	var messages []string
	observer := &SecretStoreObserver{
		Log: func(format string, args ...any) {
			messages = append(messages, fmt.Sprintf(format, args...))
		},
		Address: "https://bao:8200",
	}
	// Sequence: ready -> failing -> failing (steady) -> sealed -> ready
	observer.Observe("ready")
	observer.Observe("connection refused") // transition to failing
	observer.Observe("connection refused") // steady, no log
	observer.Observe("sealed")             // transition to sealed
	observer.Observe("ready")              // recovery
	if len(messages) != 3 {
		t.Fatalf("expected 3 log messages, got %d: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0], "WARNING") || !strings.Contains(messages[0], "probe failing") {
		t.Fatalf("first message not a failing warning: %q", messages[0])
	}
	if !strings.Contains(messages[1], "SEALED") {
		t.Fatalf("second message not sealed: %q", messages[1])
	}
	if !strings.Contains(messages[2], "recovered") {
		t.Fatalf("third message not recovery: %q", messages[2])
	}
}

// TestReadyIgnoresSecretStoreState pins the requirement that a sealed or failing
// secret store does NOT make the server NotReady — branch CI, git ingress, and
// MCP must keep working regardless of secret-store health.
func TestReadyIgnoresSecretStoreState(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:       fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Kind: "local"}}},
		Configured:  func(context.Context) error { return nil },
		Cluster:     func(context.Context) error { return nil },
		Audit:       func(context.Context) error { return nil },
		VCS:         func(context.Context, model.Upstream) error { return nil },
		SecretStore: &SecretStoreStatus{Configured: true, Address: "https://bao:8200"},
		SecretStoreProbe: func(context.Context) error {
			return errors.New("sealed")
		},
		SecretStoreSealed: func(context.Context) (bool, error) {
			return true, nil
		},
	}
	// A sealed or failing secret store must not affect readiness.
	if err := health.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() = %v; secret store state must not gate readiness", err)
	}
}
