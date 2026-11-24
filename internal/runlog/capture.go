package runlog

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"os"
	"sync"

	"github.com/oberthci/oberth/internal/publicerr"
)

// CaptureRequest binds one source read to its gap and optional storage retry policy.
// Retry waits for the caller's backoff or returns its bounded context refusal.
type CaptureRequest struct {
	Source io.ReadCloser
	Gap    Gap
	Retry  func(context.Context) error
	// Complete refuses a partial source read instead of publishing it as archived.
	Complete bool
}

// Capture takes ownership of source, closing and joining its cancellation watcher.
// It consumes this reservation once. Spooling is bounded and outside the commit
// gate; cancellation keeps the durable interrupted receipt and never reopens it.
// Gap preserves source detail supplied by C5; generated reasons stay short.
// Capture changes no execution verdict.
func (r *Reservation) Capture(ctx context.Context, input CaptureRequest) error {
	source, gap := input.Source, input.Gap
	if source != nil {
		source = &onceSource{ReadCloser: source}
		defer func() { _ = source.Close() }()
	}
	if err := r.begin(source, gap); err != nil {
		return err
	}
	defer r.store.active.Delete(copyKey(r.run, r.entry))
	if err := ctx.Err(); err != nil {
		return storage(logOperationCanceledReason, err)
	}
	spool, err := r.stage(ctx, input.Retry)
	if err != nil {
		return err
	}
	defer func() { _ = spool.Close() }()
	result := captureResult{detail: gap.Detail}
	if gap.Detail != "" || gap.Reason != "" {
		result.reason = incompleteReason
		if gap.Reason == PodMissing {
			result.reason = podMissingReason
		}
		if gap.Reason == NotRun {
			result.reason = notRunReason
		}
	}
	if source != nil {
		input.Source = source
		result, err = r.readCapture(ctx, spool, input, result)
		if err != nil {
			return err
		}
	}
	return retryCapture(ctx, input.Retry, func() error { return r.commit(ctx, spool, result) })
}
func (r *Reservation) begin(source io.ReadCloser, gap Gap) error {
	if r == nil || r.store == nil || !r.used.CompareAndSwap(false, true) {
		return publicerr.New(publicerr.InvalidTransition, captureReservationAlreadyUsedReason, captureReservationRemedy, nil)
	}
	if (source == nil && gap.Detail == "" && gap.Reason == "") || (gap.Reason != "" && gap.Reason != PodMissing && gap.Reason != NotRun) {
		r.store.active.Delete(copyKey(r.run, r.entry))
		return publicerr.New(publicerr.InvalidInput, captureSourceOrDetailRequiredReason, captureSourceRemedy, nil)
	}
	return nil
}
func copySource(ctx context.Context, destination io.Writer, source io.ReadCloser, limit int64) (int64, error) {
	finished, joined := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	closeSource := func() { closeOnce.Do(func() { _ = source.Close() }) }
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			closeSource()
		case <-finished:
		}
	}()
	size, err := io.CopyBuffer(destination, io.LimitReader(source, limit), make([]byte, 32<<10))
	close(finished)
	closeSource()
	<-joined
	return size, err
}
func (r *Reservation) commit(ctx context.Context, spool *os.File, result captureResult) error {
	s := r.store
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(r.run)
	if err != nil {
		return err
	}
	at := position(saved, r.entry)
	if at < 0 {
		return storage(containerCaptureHasNoPendingReceiptReason, nil)
	}
	if saved.Containers[at].State == Captured {
		return s.save(r.run, saved)
	}
	if saved.Containers[at].State != Copying {
		return storage(containerCaptureHasNoPendingReceiptReason, nil)
	}
	return r.commitBytes(saved, stagedCapture{spool, result})
}
func (r *Reservation) commitBytes(saved index, input stagedCapture) error {
	s := r.store
	spool, result := input.spool, input.result
	at := position(saved, r.entry)
	file, err := s.openBody(r.run, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	start, err := appendPosition(file, saved.Size)
	if err != nil {
		return storage(cannotFindLogAppendPositionReason, err)
	}
	keep := min(result.size, max(0, s.limit-start))
	if keep < result.size && result.reason != limitReason {
		result.reason = limitReason
		result.explain(limitDetail)
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return storage(cannotReadStagedContainerOutputReason, err)
	}
	written, err := io.CopyN(file, spool, keep)
	if err != nil {
		return storage(cannotCommitContainerBytesReason, err)
	}
	if err = file.Sync(); err != nil {
		return storage(cannotSyncContainerBytesReason, err)
	}
	entry := saved.Containers[at]
	entry.Start, entry.End = start, start+written
	entry.State = Captured
	entry.Reason, entry.Detail = result.reason, result.detail
	saved.Containers[at] = entry
	saved.Size = entry.End
	return s.save(r.run, saved)
}

type captureResult struct {
	size           int64
	reason, detail string
}

func (r *captureResult) explain(detail string) {
	if r.detail == "" {
		r.detail = detail
		return
	}
	r.detail += "\n" + detail
}

type onceSource struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (s *onceSource) Close() error { s.once.Do(func() { s.err = s.ReadCloser.Close() }); return s.err }

func (r *Reservation) stage(ctx context.Context, retry func(context.Context) error) (*os.File, error) {
	name := ".capture-" + rand.Text()
	var spool *os.File
	err := retryCapture(ctx, retry, func() error {
		var err error
		spool, err = r.store.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		return err
	})
	if err != nil {
		return nil, storage(cannotStageContainerOutputReason, err)
	}
	if err = retryCapture(ctx, retry, func() error { return r.store.root.Remove(name) }); err != nil {
		_ = spool.Close()
		return nil, storage(cannotUnlinkCaptureSpoolReason, err)
	}
	return spool, nil
}
func retryCapture(ctx context.Context, retry func(context.Context) error, operation func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return storage(logOperationCanceledReason, err)
		}
		err := operation()
		if err == nil || retry == nil {
			return err
		}
		if cause := retry(ctx); cause != nil {
			return errors.Join(err, cause)
		}
	}
}

type stagedCapture struct {
	spool  *os.File
	result captureResult
}

func (r *Reservation) readCapture(ctx context.Context, spool *os.File, input CaptureRequest, result captureResult) (captureResult, error) {
	var err error
	result.size, err = copySource(ctx, spool, input.Source, min(r.store.limit, math.MaxInt64-1)+1)
	if ctx.Err() != nil {
		return result, storage(logOperationCanceledReason, ctx.Err())
	}
	if err != nil && input.Complete {
		return result, storage(interruptedReason, err)
	}
	if err != nil {
		result.reason = interruptedReason
		result.explain(interruptedDetail)
		result.explain(err.Error())
	}
	return result, nil
}
