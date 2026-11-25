package sourcedelivery

import (
	"context"
	"errors"
	"io"

	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func (s *Server) recordFailure(ctx context.Context, caller Caller, started time.Time, cause error) error {
	if errors.Is(ctx.Err(), context.Canceled) || s.config.RecordFailure == nil {
		return cause
	}
	var f *Failure
	if errors.As(cause, &f) {
		return cause
	}
	var p *publicerr.Error
	if errors.As(cause, &p) && p.Code() != publicerr.Unavailable && p.Code() != publicerr.Invariant && p.Code() != publicerr.SchemaRefused {
		return cause
	}
	return errors.Join(cause, s.config.RecordFailure(ctx, caller, started))
}

type terminalFailure struct{ error }

type failureReader struct {
	io.ReadCloser
	fail func(error) error
}

func (r *failureReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = r.fail(err)
	}
	return n, err
}

func (r *failureReader) Close() error {
	if err := r.ReadCloser.Close(); err != nil {
		return r.fail(err)
	}
	return nil
}
