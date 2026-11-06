package service

import (
	"context"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type runPodDemandReader interface {
	RunPodDemand(context.Context, WorkflowRequest) (*argoworkflow.PodDemand, error)
}

type queueDemandEntry struct {
	requests *argoworkflow.PodRequests
	expires  time.Time
}

type queueDemandCache struct {
	mu      sync.Mutex
	entries map[string]queueDemandEntry
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
	key := run.ID + "/" + run.SHA + "/" + run.TestedSHA
	now := s.schedulingClock().Now()
	if cache := s.queueDemands; cache != nil {
		cache.mu.Lock()
		entry, exists := cache.entries[key]
		cache.mu.Unlock()
		if exists && now.Before(entry.expires) {
			return entry.requests.Clone()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var requests *argoworkflow.PodRequests
	if repo, err := s.repositories.Repository(ctx, run.RepoID); err == nil {
		if resolver, ok := s.runs.(interface {
			Upstream(context.Context, int64) (model.Upstream, error)
		}); ok {
			if upstream, err := resolver.Upstream(ctx, repo.UpstreamID); err == nil {
				demand, err := reader.RunPodDemand(ctx, WorkflowRequest{Run: run, Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org()})
				if err == nil {
					requests = queuePodRequests(demand)
				}
			}
		}
	}
	if cache := s.queueDemands; cache != nil && ctx.Err() == nil {
		cache.mu.Lock()
		if cache.entries == nil || len(cache.entries) >= 512 {
			cache.entries = make(map[string]queueDemandEntry)
		}
		cache.entries[key] = queueDemandEntry{requests: requests.Clone(), expires: now.Add(30 * time.Second)}
		cache.mu.Unlock()
	}
	return requests
}
