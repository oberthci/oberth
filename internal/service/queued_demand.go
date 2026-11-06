package service

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type runPodDemandReader interface {
	RunPodDemand(context.Context, WorkflowRequest) (*argoworkflow.PodDemand, error)
}

const queueDemandLimit = 512
const queueDemandReadBudget = 2 * time.Second
const queueDemandNegativeTTL = 30 * time.Second

type queueDemandKey struct{ runID, sha, testedSHA string }
type queueDemandEntry struct {
	key      queueDemandKey
	requests *argoworkflow.PodRequests
	// Only negative results expire. Successful run/SHA reads survive polling.
	expires time.Time
}
type queueDemandCache struct {
	mu      sync.Mutex
	entries map[queueDemandKey]*list.Element
	recent  list.List
	pending map[queueDemandKey]chan struct{}
}

// read coalesces concurrent misses and evicts only the least recently used
// entry. Timeouts and other failures are negative cached just like absent data.
func (cache *queueDemandCache) read(ctx context.Context, key queueDemandKey, now func() time.Time, load func() *argoworkflow.PodRequests) *argoworkflow.PodRequests {
	for {
		cache.mu.Lock()
		if element := cache.entries[key]; element != nil {
			entry := element.Value.(queueDemandEntry)
			if entry.expires.IsZero() || now().Before(entry.expires) {
				cache.recent.MoveToFront(element)
				cache.mu.Unlock()
				return entry.requests.Clone()
			}
			cache.recent.Remove(element)
			delete(cache.entries, key)
		}
		if ctx.Err() != nil {
			cache.mu.Unlock()
			return nil
		}
		if ready := cache.pending[key]; ready != nil {
			cache.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		if cache.pending == nil {
			cache.pending = make(map[queueDemandKey]chan struct{})
		}
		ready := make(chan struct{})
		cache.pending[key] = ready
		cache.mu.Unlock()

		requests := load()
		entry := queueDemandEntry{key: key, requests: requests.Clone()}
		if requests == nil {
			entry.expires = now().Add(queueDemandNegativeTTL)
		}
		cache.mu.Lock()
		if cache.entries == nil {
			cache.entries = make(map[queueDemandKey]*list.Element)
		}
		cache.entries[key] = cache.recent.PushFront(entry)
		if cache.recent.Len() > queueDemandLimit {
			oldest := cache.recent.Back()
			delete(cache.entries, oldest.Value.(queueDemandEntry).key)
			cache.recent.Remove(oldest)
		}
		delete(cache.pending, key)
		close(ready)
		cache.mu.Unlock()
		return requests
	}
}

type queueDemandBudgetKey struct{}

// The whole list shares this demand deadline, without cancelling unrelated
// row observations or the database read when the demand budget is spent.
func withQueueDemandBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	budget, cancel := context.WithTimeout(ctx, queueDemandReadBudget)
	return context.WithValue(ctx, queueDemandBudgetKey{}, budget), cancel
}

func queuePodRequests(demand *argoworkflow.PodDemand) *argoworkflow.PodRequests {
	if demand == nil || !demand.Valid() {
		return nil
	}
	return &argoworkflow.PodRequests{CPUMilli: demand.CPUMilli, MemoryBytes: demand.MemoryBytes, Pods: demand.Pods}
}

// Called only after run authorization, including on cache hits. Failed sizing
// leaves figures unknown; it never changes admission or invents a zero demand.
func (s *API) queuedRequests(ctx context.Context, run model.Run) *argoworkflow.PodRequests {
	reader, ok := s.resourceDeclarations.(runPodDemandReader)
	if !ok || s.repositories == nil {
		return nil
	}
	key := queueDemandKey{runID: run.ID, sha: run.SHA, testedSHA: run.TestedSHA}
	if budget, ok := ctx.Value(queueDemandBudgetKey{}).(context.Context); ok {
		ctx = budget
	}
	ctx, cancel := context.WithTimeout(ctx, queueDemandReadBudget)
	defer cancel()
	load := func() *argoworkflow.PodRequests {
		if ctx.Err() != nil {
			return nil
		}
		if repo, err := s.repositories.Repository(ctx, run.RepoID); err == nil {
			if resolver, ok := s.runs.(interface {
				Upstream(context.Context, int64) (model.Upstream, error)
			}); ok {
				if upstream, err := resolver.Upstream(ctx, repo.UpstreamID); err == nil {
					demand, err := reader.RunPodDemand(ctx, WorkflowRequest{Run: run, Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org()})
					if err == nil {
						return queuePodRequests(demand)
					}
				}
			}
		}
		return nil
	}
	if s.queueDemands == nil {
		return load()
	}
	return s.queueDemands.read(ctx, key, s.schedulingClock().Now, load)
}
