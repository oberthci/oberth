package argojob

import (
	"context"
	"errors"
	"time"

	"golang.org/x/time/rate"
)

const (
	collectionBatch       = 8
	collectionInterval    = 15 * time.Second
	CollectionClientQPS   = 5
	CollectionClientBurst = 1
	archiveRetryBase      = time.Minute
	archiveRetryMax       = time.Hour
)

var (
	errCollectionPending = errors.New("collection pass limit reached")
	errArchivePending    = errors.New("step logs are not archived")
)

// Collection has its own read budget; live observation keeps the server budget.
func (e *Executor) collectionRead(ctx context.Context) error { return e.collectionReads.Wait(ctx) }

func (e *Executor) collectionDelay(t *tracking) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.config.Now()
	if now.Before(t.archiveRetryAt) {
		return t.archiveRetryAt.Sub(now)
	}
	if !now.Before(e.collectionWindow) {
		e.collectionWindow, e.collectionCount = now.Add(collectionInterval), 0
	}
	if e.collectionCount >= collectionBatch {
		return e.collectionWindow.Sub(now)
	}
	e.collectionCount++
	return 0
}

func (e *Executor) retryArchive(t *tracking, cause error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.retryArchiveLocked(t, cause)
}

func (e *Executor) retryArchiveLocked(t *tracking, cause error) {
	delay := min(archiveRetryBase<<min(t.archiveFailures, 6), archiveRetryMax)
	t.archiveFailures++
	t.archiveRetryAt = e.config.Now().Add(delay)
	e.queue.AddAfter(t.binding.Workflow.Name, delay)
	e.config.Logger.Warn("run log archive deferred", "run_id", t.binding.RunID, "retry_after", delay.String(), "cause", cause)
}

func (e *Executor) archivePending(t *tracking, err error) {
	if errors.Is(err, errCollectionPending) {
		e.queue.AddAfter(t.binding.Workflow.Name, collectionInterval)
		return
	}
	e.mu.Lock()
	copying := t.capturing > 0 || len(t.pending) > 0
	e.mu.Unlock()
	if errors.Is(err, errArchivePending) && copying {
		e.queue.AddAfter(t.binding.Workflow.Name, time.Second)
		return
	}
	e.retryArchive(t, err)
}

func newCollectionReads() *rate.Limiter {
	return rate.NewLimiter(CollectionClientQPS, CollectionClientBurst)
}
