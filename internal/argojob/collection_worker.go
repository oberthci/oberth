package argojob

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/nativebinding"
)

func (e *Executor) discoverRun(ctx context.Context, name string) {
	if e.collector != nil {
		e.collector.queue.Add(name)
	}
	e.discoverChild(ctx, name)
}

func (e *Executor) collectionDue(t *tracking) bool {
	if !t.archiveOnly {
		return true
	}
	if e.collector != nil {
		e.collectFinished(t)
		e.retire(t)
		return false
	}
	if delay := e.collectionDelay(t); delay > 0 {
		e.queue.AddAfter(t.binding.Workflow.Name, delay)
		return false
	}
	return true
}

// Collection owns its tracking, retry queue, API reads and one copy worker.
// No collection wait consumes live observation or live capture capacity.
func (e *Executor) runCollection(ctx context.Context) {
	logsDone := e.startCaptureWorkers(ctx, 1)
	defer func() { <-logsDone }()
	for {
		name, closed := e.queue.Get()
		if closed {
			return
		}
		e.mu.Lock()
		t := e.tracked[name]
		e.mu.Unlock()
		if t == nil && !e.liveWatching(name) {
			if !e.resumeCollection(ctx, name) {
				e.queue.Forget(name)
			}
		} else if t != nil {
			e.process(ctx, name)
		}
		e.queue.Done(name)
	}
}

func (e *Executor) liveWatching(name string) bool {
	if e.live == nil {
		return false
	}
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	return e.live.tracked[name] != nil
}

func (e *Executor) collectFinished(t *tracking) {
	if e.collector == nil || !t.completed {
		return
	}
	historical := t.historical
	if e.config.Cache != nil {
		if w, ok := e.config.Cache.Workflow(t.binding.Workflow.Name); ok && string(w.UID) == t.binding.Workflow.UID {
			historical = w.Spec.TTLStrategy == nil && !slices.Contains(w.Finalizers, nativebinding.RecordFinalizer)
		}
	}
	seed := trackingSeed{child: t.child, completed: true, archiveOnly: true, historical: historical}
	if t.settled && !e.expired(t) {
		seed.archiveEvidenceUntil = t.evidenceUntil
	}
	e.collector.track(t.binding, t.plan, seed)
}

// Newly settled live observations keep their existing bounded transient
// attempts. Only a failed durability check can invalidate their evidence;
// waiting behind collection work cannot do so.
func (e *Executor) liveArchivePending(ctx context.Context, t *tracking, cause error) bool {
	var check *archiveCheckError
	if errors.As(cause, &check) && !t.archiveEvidenceUntil.IsZero() && !e.config.Now().Before(t.archiveEvidenceUntil) {
		if err := e.config.Progress.AdvanceNative(ctx, t.binding.RunID); err != nil {
			e.logError(ctx, t.binding.RunID, err)
		} else {
			t.archiveEvidenceUntil = time.Time{}
			e.config.Metrics.IncompleteEvidence.Add(1)
		}
	}
	if !t.archiveLive {
		return false
	}
	t.refusals++
	if t.refusals < observationAttempts {
		e.queue.AddRateLimited(t.binding.Workflow.Name)
		return true
	}
	t.archiveLive = false
	return false
}
