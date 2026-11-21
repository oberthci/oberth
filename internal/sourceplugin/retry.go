package sourceplugin

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"
)

// A connection that never opened, or was lost before headers, resumes this
// request: no server read it. While the server is replaced the Service may still
// route to the departing Pod, so a dial can time out or find no host, not only
// be refused. An unknown host name, trust failures and a failed or incomplete
// body remain terminal for this Load.
func retryConnection(err error) bool {
	var name *net.DNSError
	if errors.As(err, &name) && name.IsNotFound {
		return false
	}
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *Client) retryWait(ctx context.Context, delay time.Duration, next *time.Time) error {
	for delay > 0 {
		c.reportWaiting(next)
		part := min(delay, max(next.Sub(c.now()), time.Nanosecond))
		if err := c.wait(ctx, part); err != nil {
			return err
		}
		delay -= part
	}
	c.reportWaiting(next)
	return ctx.Err()
}
func (c *Client) reportWaiting(next *time.Time) {
	if now := c.now(); !now.Before(*next) {
		if c.log != nil {
			c.log(reasonWaiting)
		}
		*next = now.Add(30 * time.Second)
	}
}
