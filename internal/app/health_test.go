package app

import (
	"context"
	"errors"
	"strings"
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
		AuditChain: func(context.Context) (AuditChainStatus, error) {
			return AuditChainStatus{HeadID: 41, HeadSHA256: "aa11", AnchorID: 7, TSAURL: "https://tsa.example", AnchoredAt: "2026-08-08T10:00:00Z"}, nil
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
}
