package service

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const listDiagnosticLimit = 512
const listDiagnosticWorkers = 4
const listDiagnosticTTL = 10 * time.Second
const executionDiagnosticTTL = 2 * time.Second

type listDiagnosticKey struct {
	id, sha, testedSHA, workflow, scope, kind string
	updated                                   int64
}
type listDiagnosticValue struct {
	scheduling *model.RunScheduling
	ready      <-chan struct{}
}
type listDiagnosticEntry struct {
	key       listDiagnosticKey
	value     listDiagnosticValue
	checkedAt time.Time
}
type listDiagnosticTask struct {
	key  listDiagnosticKey
	load func(context.Context) listDiagnosticValue
	done chan struct{}
}
type listDiagnosticCache struct {
	mu      sync.Mutex
	entries map[listDiagnosticKey]*list.Element
	pending map[listDiagnosticKey]chan struct{}
	recent  list.List
	queue   []listDiagnosticTask
	workers int
}

// Workers own diagnostic I/O; readers join only a short page-wide grace.
// Refreshes are bounded, coalesced and
// joined by API.Shutdown. Keys bind the durable attempt and the authorized view;
// a scoped reader cannot reuse an administrator's scheduling projection.
func (s *API) listDiagnostic(run model.Run, kind string, load func(context.Context) listDiagnosticValue) listDiagnosticValue {
	cache := s.listDiagnostics
	if cache == nil {
		return listDiagnosticValue{}
	}
	scope := ""
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
		cache.recent.MoveToFront(element)
		ttl := listDiagnosticTTL
		if kind == "execution" {
			ttl = executionDiagnosticTTL
		}
		if load == nil || now.Sub(entry.checkedAt) < ttl {
			return result
		}
	}
	if load == nil {
		return result
	}
	if done := cache.pending[key]; done != nil {
		result.ready = done
		return result
	}
	if len(cache.pending) >= listDiagnosticLimit || s.lifetime.Err() != nil {
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
		cache.pending = make(map[listDiagnosticKey]chan struct{})
	}
	done := make(chan struct{})
	cache.pending[key] = done
	cache.queue = append(cache.queue, listDiagnosticTask{key: key, load: load, done: done})
	result.ready = done
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
				close(task.done)
				delete(cache.pending, task.key)
			}
			cache.queue = nil
			return
		}
		task := cache.queue[0]
		cache.queue[0] = listDiagnosticTask{}
		cache.queue = cache.queue[1:]
		cache.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.lifetime, 2*time.Second)
		value := task.load(ctx)
		cancel()
		cache.mu.Lock()
		delete(cache.pending, task.key)
		close(task.done)
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

func (s *API) cachedRunListExecution(run model.Run) listDiagnosticValue {
	return s.listDiagnostic(run, "execution", func(ctx context.Context) listDiagnosticValue {
		return listDiagnosticValue{scheduling: s.observeRunListExecution(ctx, run)}
	})
}

// One shared read budget per page, not per row. Background workers still own
// all I/O and shutdown. Fast native reads appear on the first response; slow
// or unavailable Argo never turns a list into N serial Kubernetes deadlines.
func waitListExecution(ctx context.Context, pending []<-chan struct{}) {
	if len(pending) == 0 {
		return
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for _, done := range pending {
		select {
		case <-done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
