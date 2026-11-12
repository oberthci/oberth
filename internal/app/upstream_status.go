package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type vcsObservation struct {
	state     string
	lastError string
	failures  int
	checkedAt time.Time
	next      time.Time
}

// VCSSnapshot is diagnostic only. Endpoint observations retain their own retry
// clock and credential binding; readers never initiate network activity.
type VCSSnapshot struct {
	key          string
	mu           sync.Mutex
	results      []UpstreamStatus
	observations map[string]vcsObservation
	inflight     chan struct{}
}

// SSH authentication does not depend on the organization path. Keep username,
// verification address and credentials distinct: known_hosts matches host and
// port spellings, even when they resolve to the same network endpoint.
func upstreamStatusKey(u model.Upstream, identity string) string {
	if u.Kind != "local" {
		_, _, address, err := sshEndpoint(u.BaseURL)
		parsed, parseErr := url.Parse(u.BaseURL)
		if err == nil && parseErr == nil {
			user := "git"
			if parsed.User != nil && parsed.User.Username() != "" {
				user = parsed.User.Username()
			}
			return fmt.Sprintf("ssh:%q:%q:%q", address, user, identity)
		}
	}
	return fmt.Sprintf("upstream:%q:%q:%q", u.Kind, u.BaseURL, identity)
}

func vcsState(results []UpstreamStatus) string {
	state := "ready"
	for _, result := range results {
		switch result.Probe {
		case "ready", "archived":
		case "checking":
			state = "checking"
		default:
			return "unavailable"
		}
	}
	return state
}

type upstreamSample struct {
	probe       func(context.Context) error
	identityErr error
	observation vcsObservation
}

func (health Health) probeVCS(ctx context.Context, upstreams []model.Upstream) []UpstreamStatus {
	cache := health.VCSCache
	if cache == nil {
		cache = &VCSSnapshot{}
	}
	cache.mu.Lock()
	if cache.inflight != nil {
		done := cache.inflight
		cache.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return append([]UpstreamStatus(nil), cache.results...)
	}
	done := make(chan struct{})
	cache.inflight = done
	previous := make(map[string]vcsObservation, len(cache.observations))
	for key, value := range cache.observations {
		previous[key] = value
	}
	cache.mu.Unlock()

	config := health.StatusProbes.withDefaults()
	samples := make(map[string]*upstreamSample)
	keys := make([]string, len(upstreams))
	for i, u := range upstreams {
		identity, err := u.KeyName, error(nil)
		probe := func(ctx context.Context) error { return health.VCS(ctx, u) }
		if health.PrepareVCS != nil {
			identity, probe, err = health.PrepareVCS(ctx, u)
		}
		key := upstreamStatusKey(u, identity)
		if err != nil {
			key = "identity-error:" + upstreamProbeKey([]model.Upstream{u})
		}
		keys[i] = key
		if _, found := samples[key]; !found {
			samples[key] = &upstreamSample{probe: probe, identityErr: err, observation: previous[key]}
		}
	}
	health.sampleUpstreamGroups(ctx, samples, config)
	results := make([]UpstreamStatus, len(upstreams))
	observations := make(map[string]vcsObservation, len(samples))
	for i, u := range upstreams {
		o := samples[keys[i]].observation
		observations[keys[i]] = o
		results[i] = UpstreamStatus{ID: u.ID, Name: u.Name, Kind: u.Kind, BaseURL: u.BaseURL, Probe: o.state, CheckedAt: o.checkedAt, LastError: o.lastError, ConsecutiveFailures: o.failures}
	}
	cache.mu.Lock()
	if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		cache.results, cache.observations, cache.key = results, observations, upstreamProbeKey(upstreams)
	}
	cache.inflight = nil
	results = append([]UpstreamStatus(nil), cache.results...)
	close(done)
	cache.mu.Unlock()
	return results
}

func (health Health) sampleUpstreamGroups(ctx context.Context, samples map[string]*upstreamSample, config StatusProbeConfig) {
	aggregate, cancel := context.WithTimeout(ctx, vcsProbeAggregate)
	defer cancel()
	jobs := make(chan *upstreamSample, len(samples))
	now := time.Now()
	for _, sample := range samples {
		if !sample.observation.next.After(now) {
			jobs <- sample
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(jobs)) {
		workers.Go(func() {
			for sample := range jobs {
				probeCtx, stop := context.WithTimeout(aggregate, vcsProbeTimeout)
				err := sample.identityErr
				if err == nil {
					err = probeCtx.Err()
				}
				if err == nil {
					err = sample.probe(probeCtx)
				}
				if err == nil {
					err = probeCtx.Err()
				}
				stop()
				o := &sample.observation
				o.checkedAt = time.Now().UTC()
				if err == nil {
					o.state, o.lastError, o.failures = "ready", "", 0
				} else {
					o.failures++
					o.lastError = boundedDetail(err.Error())
					if o.failures >= config.FailureThreshold {
						o.state = "unavailable"
					} else if o.state != "ready" {
						o.state = "checking"
					}
				}
				o.next = o.checkedAt.Add(upstreamRetryDelay(config.UpstreamInterval, o.failures))
			}
		})
	}
	workers.Wait()
}
