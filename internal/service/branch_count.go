package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// branchCounter reports how many branches a cached mirror holds besides its
// default branch (#1508). The Git cache implements it; a RefResolver without
// it simply leaves the count unknown.
type branchCounter interface {
	NonDefaultBranchCount(ctx context.Context, repo string) (int, error)
}

const (
	// branchCountTTL bounds how stale the /repos counter may be. Dashboard
	// polls within the window reuse the cached figure instead of walking refs.
	branchCountTTL = time.Minute
	// branchCountBudget caps the time one listing spends refreshing expired
	// counts; repositories it does not reach stay unknown until the next read.
	branchCountBudget = 2 * time.Second
	branchCountLimit  = 4096
)

type branchCountEntry struct {
	count   int
	known   bool
	expires time.Time
}

// branchCountCache holds per-mirror counts shared by every per-actor API copy.
// Failures (a mirror not cached yet) are cached as unknown for the same TTL so
// a missing mirror never costs a Git command per render.
type branchCountCache struct {
	// refresh serialises loading so concurrent listings never fan out the
	// same ref walk twice; mu guards entries only.
	refresh sync.Mutex
	mu      sync.Mutex
	entries map[string]branchCountEntry
	now     func() time.Time
}

func (cache *branchCountCache) clock() time.Time {
	if cache.now != nil {
		return cache.now()
	}
	return time.Now()
}

func (cache *branchCountCache) lookup(key string) (branchCountEntry, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok || !cache.clock().Before(entry.expires) {
		return branchCountEntry{}, false
	}
	return entry, true
}

func (cache *branchCountCache) store(key string, entry branchCountEntry) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil || len(cache.entries) >= branchCountLimit {
		cache.entries = make(map[string]branchCountEntry)
	}
	cache.entries[key] = entry
}

// counts returns the cached or freshly loaded count per key; a key with no
// known count is absent from the result.
func (cache *branchCountCache) counts(ctx context.Context, keys []string, load func(context.Context, string) (int, error)) map[string]int {
	result := make(map[string]int, len(keys))
	var missing []string
	for _, key := range keys {
		if entry, ok := cache.lookup(key); ok {
			if entry.known {
				result[key] = entry.count
			}
			continue
		}
		missing = append(missing, key)
	}
	if len(missing) == 0 {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, branchCountBudget)
	defer cancel()
	cache.refresh.Lock()
	defer cache.refresh.Unlock()
	for _, key := range missing {
		// Another listing may have loaded it while this one waited.
		if entry, ok := cache.lookup(key); ok {
			if entry.known {
				result[key] = entry.count
			}
			continue
		}
		if ctx.Err() != nil {
			break
		}
		count, err := load(ctx, key)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil) {
			// Budget exhausted, not a property of the mirror: retry next read.
			break
		}
		entry := branchCountEntry{count: count, known: err == nil, expires: cache.clock().Add(branchCountTTL)}
		cache.store(key, entry)
		if entry.known {
			result[key] = count
		}
	}
	return result
}

// nonDefaultBranchCounts maps repository ID to its mirror's non-default
// branch count. Unknown counts (no counter, mirror not cached, budget spent)
// are absent so callers render them as unknown rather than zero.
func (service *API) nonDefaultBranchCounts(ctx context.Context, repos []model.Repository) map[int64]int {
	counter, ok := service.refs.(branchCounter)
	if !ok || len(repos) == 0 {
		return nil
	}
	cache := service.branchCounts
	if cache == nil {
		cache = &branchCountCache{}
	}
	inputs := make(map[int64]string, len(repos))
	keys := make([]string, 0, len(repos))
	for _, repo := range repos {
		input := service.cacheInputFor(ctx, repo)
		inputs[repo.ID] = input
		keys = append(keys, input)
	}
	counts := cache.counts(ctx, keys, counter.NonDefaultBranchCount)
	result := make(map[int64]int, len(counts))
	for id, input := range inputs {
		if count, ok := counts[input]; ok {
			result[id] = count
		}
	}
	return result
}

func branchCountPointer(counts map[int64]int, id int64) *int {
	count, ok := counts[id]
	if !ok {
		return nil
	}
	return &count
}
