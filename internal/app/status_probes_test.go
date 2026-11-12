package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestStatusNeverInitiatesProbes(t *testing.T) {
	health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "forge"}}}, SecretStore: &SecretStoreStatus{Configured: true}}
	health.Cluster = func(context.Context) error { t.Fatal("request probed Kubernetes"); return nil }
	health.VCS = func(context.Context, model.Upstream) error { t.Fatal("request probed upstream"); return nil }
	health.SecretStoreProbe = func(context.Context) error { t.Fatal("request probed OpenBao"); return nil }
	for _, configured := range []bool{false, true} {
		if configured {
			health.VCSCache = &VCSSnapshot{}
			health.ClusterCache = &ClusterSnapshot{}
			health.SecretStoreCache = &SecretStoreProbeSnapshot{}
		}
		value, err := health.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		status := value.(HealthStatus)
		if status.Cluster != "unavailable" || status.VCS != "unavailable" || status.SecretStore.Probe != "unavailable" {
			t.Fatalf("unsampled diagnostics claimed ready: %+v", status)
		}
		if _, err := health.ScopedStatus(t.Context(), api.Actor{Scope: []string{"forge"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusProbeLifecycleAndTimestamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		probe := func(ctx context.Context) error { calls.Add(1); <-ctx.Done(); return ctx.Err() }
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "forge"}}},
			VCSCache: &VCSSnapshot{}, ClusterCache: &ClusterSnapshot{}, SecretStoreCache: &SecretStoreProbeSnapshot{},
			VCS: func(ctx context.Context, _ model.Upstream) error { return probe(ctx) }, Cluster: probe, SecretStoreProbe: probe, SecretStore: &SecretStoreStatus{Configured: true}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); health.WatchStatusProbes(ctx) }()
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("independent startup probes missing", calls.Load())
		}
		// Reads, including scoped status, neither join nor multiply inflight probes.
		for range 10 {
			health.Status(t.Context())
			health.ScopedStatus(t.Context(), api.Actor{Scope: []string{"forge"}})
		}
		if calls.Load() != 3 {
			t.Fatal("reader initiated probes")
		}
		time.Sleep(secretStoreProbeTimeout)
		synctest.Wait()
		value, err := health.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		status := value.(HealthStatus)
		if status.ClusterCheckedAt.IsZero() || status.SecretStore.CheckedAt.IsZero() || len(status.UpstreamInfo) != 1 || status.UpstreamInfo[0].CheckedAt.IsZero() {
			t.Fatalf("probe timestamps absent: %+v", status)
		}
		cancel()
		synctest.Wait()
		<-done
	})
}

func TestStatusProbeDoesNotReuseChangedIdentity(t *testing.T) {
	upstreams := []model.Upstream{{ID: 1, Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/org", KeyName: "first"}}
	health := Health{VCSCache: &VCSSnapshot{}, VCS: func(context.Context, model.Upstream) error { return nil }}
	health.probeVCS(t.Context(), upstreams)
	initial := health.vcsStatus(upstreams)
	if initial[0].Probe != "ready" || initial[0].CheckedAt.IsZero() {
		t.Fatal(initial)
	}
	upstreams[0].KeyName = "second"
	if got := health.vcsStatus(upstreams); got[0].Probe != "unavailable" || !got[0].CheckedAt.IsZero() {
		t.Fatal("reused previous key's observation", got)
	}
	health.VCS = func(context.Context, model.Upstream) error { return errors.New("key refused") }
	health.probeVCS(t.Context(), upstreams)
	if got := health.vcsStatus(upstreams); got[0].Probe == "ready" {
		t.Fatal(got)
	}
	// Old observations retain their actual time instead of reporting request time.
	health.VCSCache.mu.Lock()
	health.VCSCache.results[0].CheckedAt = time.Now().Add(-time.Hour)
	health.VCSCache.mu.Unlock()
	if got := health.vcsStatus(upstreams); got[0].CheckedAt.IsZero() || got[0].CheckedAt != health.VCSCache.results[0].CheckedAt {
		t.Fatal(got)
	}
}

func TestStatusProbesRefreshEveryIntervalAfterSlowSamples(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var vcs, cluster, secret atomic.Int32
		probe := func(counter *atomic.Int32) error {
			counter.Add(1)
			time.Sleep(time.Second)
			return nil
		}
		health := Health{
			Store:    fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "forge"}}},
			VCSCache: &VCSSnapshot{}, ClusterCache: &ClusterSnapshot{}, SecretStoreCache: &SecretStoreProbeSnapshot{},
			VCS:              func(context.Context, model.Upstream) error { return probe(&vcs) },
			Cluster:          func(context.Context) error { return probe(&cluster) },
			SecretStore:      &SecretStoreStatus{Configured: true},
			SecretStoreProbe: func(context.Context) error { return probe(&secret) },
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); health.WatchStatusProbes(ctx) }()
		synctest.Wait()
		time.Sleep(secretStoreProbeTTL + 2*time.Second)
		synctest.Wait()
		cancel()
		synctest.Wait()
		<-done
		if vcs.Load() != 4 || cluster.Load() != 4 || secret.Load() != 2 {
			t.Fatalf("samples skipped scheduled ticks: vcs=%d cluster=%d secret=%d", vcs.Load(), cluster.Load(), secret.Load())
		}
	})
}
