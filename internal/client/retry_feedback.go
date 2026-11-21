package client

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/restartretry"
	"io"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

type readFeedback struct {
	since           time.Time
	lost, announced bool
}

func (f readFeedback) failure(d apicontract.Definition, err error) error {
	if err == nil {
		return nil
	}
	if f.lost && (errors.Is(err, context.DeadlineExceeded) || restartretry.Transient(err)) {
		return failure("no_reply", err)
	}
	return requestFailure(d, err)
}

func (c *Client) retryWait(ctx context.Context, delay time.Duration, f *readFeedback) error {
	if err := c.wait(ctx, delay); err != nil {
		return err
	}
	if f.lost && !f.announced && c.now().Sub(f.since) >= 5*time.Second {
		f.announced = true
		_, err := fmt.Fprintln(c.diagnostic, "No reply yet, trying again.")
		return err
	}
	return nil
}

// Diagnostics can be emitted by concurrent MCP calls and the session stream.
type diagnosticWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *diagnosticWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(data)
}

func (f readFeedback) beforeSendFailure(d apicontract.Definition, err error) error {
	if d.Mutation {
		return failure(failureClass(err), err)
	}
	return f.failure(d, err)
}
