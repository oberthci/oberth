package installer

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestClosedEndpointTransportRejectsLateDialCallbacks(t *testing.T) {
	t.Parallel()
	transport := newEndpointTransport(nil, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("closed transport invoked its dial callback")
		return nil, nil
	})
	transport.close()
	conn, err := transport.dial(t.Context(), "tcp", "server.oberth.ci:443")
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("closed transport admitted a late dial", err)
	}
}
