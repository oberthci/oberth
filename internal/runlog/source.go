package runlog

import (
	"context"
	"errors"
	"io"
)

// SourceCapture opens one verified native source after its durable reservation.
// Retry belongs to the caller's bounded observation window.
type SourceCapture struct {
	RunID     string
	Container ContainerLog
	Open      func(context.Context) (io.ReadCloser, error)
	Retry     func(context.Context) error
}

// Capture retries reservation and source availability, then retains one source read.
// Even failed opens consume their receipt, leaving an explicit gap or interruption.
func Capture(ctx context.Context, store interface {
	Reserve(context.Context, string, ContainerLog) (*Reservation, error)
}, in SourceCapture) error {
	var reservation *Reservation
	err := retryCapture(ctx, in.Retry, func() error { var err error; reservation, err = store.Reserve(ctx, in.RunID, in.Container); return err })
	if err != nil || reservation == nil {
		return err
	}
	request := CaptureRequest{Retry: in.Retry}
	openError := retryCapture(ctx, in.Retry, func() error { var err error; request.Source, err = in.Open(ctx); return err })
	if openError != nil {
		request.Gap.Detail = openError.Error()
	}
	return errors.Join(openError, reservation.Capture(ctx, request))
}
