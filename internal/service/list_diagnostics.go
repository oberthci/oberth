package service

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const listDiagnosticLimit = 512
const listDiagnosticWorkers = 4
const listDiagnosticTTL = 10 * time.Second

type listDiagnosticKey struct {
	id, sha, testedSHA, workflow, scope, kind string
	updated                                   int64
}
type listDiagnosticValue struct {
	scheduling *model.RunScheduling
	requests   *argoworkflow.PodRequests
}
type listDiagnosticEntry struct {
	key       listDiagnosticKey
	value     listDiagnosticValue
	checkedAt time.Time
}
type listDiagnosticTask struct {
	key  listDiagnosticKey
	load func(context.Context) listDiagnosticValue
}
type listDiagnosticCache struct {
	mu      sync.Mutex
	entries map[listDiagnosticKey]*list.Element
	pending map[listDiagnosticKey]bool
	recent  list.List
	queue   []listDiagnosticTask
	workers int
}
type listDiagnosticContextKey struct{}

// Readers never wait for diagnostic I/O. Refreshes are bounded, coalesced and
// joined by API.Shutdown. Keys bind the durable attempt and the authorized view;
// a scoped reader cannot reuse an administrator's scheduling projection.
func (s *API) listDiagnostic(run model.Run, kind string, load func(context.Context) listDiagnosticValue) listDiagnosticValue {
	cache := s.listDiagnostics
	if cache == nil {
		return listDiagnosticValue{}
	}
	scope := fmt.Sprint(s.redactPressure)
	if s.scope != nil {
		scope += fmt.Sprint(s.scope.repoIDs)
	}
	key := listDiagnosticKey{id: run.ID, sha: run.SHA, testedSHA: run.TestedSHA, workflow: run.WorkflowName, updated: run.UpdatedAt.UnixNano(), scope: scope, kind: kind}
	now := s.schedulingClock().Now()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	var result listDiagnosticValue
	if element := cache.entries[key]; element != nil {
		entry := element.Value.(listDiagnosticEntry)
		result = entry.value
		if result.scheduling != nil {
			copy := *result.scheduling
			copy.AgeSeconds = max(0, int64(now.Sub(copy.ObservedAt)/time.Second))
			result.scheduling = &copy
		}
		result.requests = result.requests.Clone()
		cache.recent.MoveToFront(element)
		if now.Sub(entry.checkedAt) < listDiagnosticTTL {
			return result
		}
	}
	if cache.pending[key] || len(cache.pending) >= listDiagnosticLimit || s.lifetime.Err() != nil {
		return result
	}
	if cache.workers < listDiagnosticWorkers {
		if !s.startBackground() {
			return result
		}
		cache.workers++
		go s.refreshListDiagnostics()
	}
	if cache.pending == nil {
		cache.pending = make(map[listDiagnosticKey]bool)
	}
	cache.pending[key] = true
	cache.queue = append(cache.queue, listDiagnosticTask{key: key, load: load})
	return result
}

// A bounded FIFO lets later rows receive observations even when every request
// arrives after the first rows' snapshots expire. Only workers own API tasks.
func (s *API) refreshListDiagnostics() {
	defer s.background.Done()
	cache := s.listDiagnostics
	cache.mu.Lock()
	defer cache.mu.Unlock()
	defer func() { cache.workers-- }()
	for len(cache.queue) > 0 {
		if s.lifetime.Err() != nil {
			for _, task := range cache.queue {
				delete(cache.pending, task.key)
			}
			cache.queue = nil
			return
		}
		task := cache.queue[0]
		cache.queue[0] = listDiagnosticTask{}
		cache.queue = cache.queue[1:]
		cache.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.lifetime, queueDemandReadBudget)
		value := task.load(ctx)
		cancel()
		cache.mu.Lock()
		delete(cache.pending, task.key)
		if cache.entries == nil {
			cache.entries = make(map[listDiagnosticKey]*list.Element)
		}
		if old := cache.entries[task.key]; old != nil {
			cache.recent.Remove(old)
		}
		cache.entries[task.key] = cache.recent.PushFront(listDiagnosticEntry{key: task.key, value: value, checkedAt: s.schedulingClock().Now()})
		if cache.recent.Len() > listDiagnosticLimit {
			oldest := cache.recent.Back()
			delete(cache.entries, oldest.Value.(listDiagnosticEntry).key)
			cache.recent.Remove(oldest)
		}
	}
}

func (s *API) cachedRunListStartup(run model.Run) *model.RunScheduling {
	return s.listDiagnostic(run, "startup", func(ctx context.Context) listDiagnosticValue {
		return listDiagnosticValue{scheduling: s.observeRunListStartup(ctx, run)}
	}).scheduling
}
