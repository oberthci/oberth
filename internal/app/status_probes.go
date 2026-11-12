package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ClusterSnapshot holds diagnostic observations only; it never authorizes work.
type ClusterSnapshot struct {
	mu        sync.Mutex
	state     string
	checkedAt time.Time
}

func (health Health) clusterStatus() (string, time.Time) {
	if cache := health.ClusterCache; cache != nil {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.state == "" {
			return "unavailable", time.Time{}
		}
		return cache.state, cache.checkedAt
	}

	return "unavailable", time.Time{}
}

func upstreamProbeKey(upstreams []model.Upstream) string {
	// Include transport/key configuration, so edits never reuse a successful
	// observation of the previous endpoint or authentication identity.
	return fmt.Sprintf("%#v", upstreams)
}

func (health Health) vcsStatus(upstreams []model.Upstream) []UpstreamStatus {
	if health.VCSCache == nil {
		results := make([]UpstreamStatus, 0, len(upstreams))
		for _, u := range upstreams {
			results = append(results, UpstreamStatus{ID: u.ID, Name: u.Name, Kind: u.Kind, BaseURL: u.BaseURL, Probe: "unavailable"})
		}
		return results
	}
	cache := health.VCSCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	key := upstreamProbeKey(upstreams)
	results := make([]UpstreamStatus, 0, len(upstreams))
	for _, upstream := range upstreams {
		value := UpstreamStatus{ID: upstream.ID, Name: upstream.Name, Kind: upstream.Kind, BaseURL: upstream.BaseURL, Probe: "unavailable"}
		for _, result := range cache.results {
			if cache.key != key {
				break
			}
			if result.ID == upstream.ID && result.Name == upstream.Name && result.Kind == upstream.Kind && result.BaseURL == upstream.BaseURL {
				value = result
				break
			}
		}
		results = append(results, value)
	}
	return results
}

func (health Health) secretStoreStatus() (string, time.Time) {
	if health.SecretStoreCache == nil {
		return "unavailable", time.Time{}
	}
	cache := health.SecretStoreCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.result == "" {
		return "unavailable", time.Time{}
	}
	return cache.result, cache.age
}

// WatchStatusProbes samples independently of readers, immediately at startup.
// Separate loops keep a slow forge from delaying Kubernetes or OpenBao samples.
// The owner cancels and joins this watcher before closing its store.
func (health Health) WatchStatusProbes(ctx context.Context) {
	config := health.StatusProbes.withDefaults()
	var workers sync.WaitGroup
	watch := func(interval time.Duration, sample func(context.Context)) {
		workers.Go(func() {

			for ctx.Err() == nil {
				sample(ctx)
				timer := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}
	if health.VCSCache != nil && health.VCS != nil && health.Store != nil {
		watch(config.UpstreamInterval, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, vcsProbeAggregate)
			defer cancel()
			upstreams, err := health.Store.ListUpstreams(ctx)
			if err != nil {
				return
			}
			repositories, err := health.Store.ListRepositories(ctx)
			if err != nil {
				return
			}
			selected := activeProbeUpstreams(upstreams, repositories)
			health.probeVCS(ctx, selected)
		})
	}
	if health.ClusterCache != nil && health.Cluster != nil {
		watch(config.ClusterInterval, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, vcsProbeTimeout)
			defer cancel()
			state := "unavailable"
			if health.Cluster(ctx) == nil {
				state = "ready"
			}
			cache := health.ClusterCache
			cache.mu.Lock()
			cache.state, cache.checkedAt = state, time.Now().UTC()
			cache.mu.Unlock()
		})
	}
	if health.SecretStoreCache != nil && health.SecretStoreProbe != nil {
		observer := &SecretStoreObserver{Log: health.Log}
		if health.SecretStore != nil {
			observer.Address = health.SecretStore.Address
		}
		watch(config.SecretStoreInterval, func(ctx context.Context) {
			result := health.RefreshSecretStore(ctx)
			if health.Log != nil {
				observer.Observe(result)
			}
		})
	}
	workers.Wait()
}
