package installer

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
)

// endpointTransport owns dial callbacks until verification ends. Closing first
// stops new callbacks, then cancels pending dials and joins callbacks in flight.
type endpointTransport struct {
	http    *http.Transport
	mu      sync.Mutex
	closed  bool
	active  sync.WaitGroup
	connect func(context.Context, string, string) (net.Conn, error)
}

func newEndpointTransport(config *tls.Config, dial func(context.Context, string, string) (net.Conn, error)) *endpointTransport {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	t := &endpointTransport{http: &http.Transport{TLSClientConfig: config}, connect: dial}
	t.http.DialContext = t.dial
	return t
}

func (t *endpointTransport) dial(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, context.Canceled
	}
	t.active.Add(1)
	t.mu.Unlock()
	defer t.active.Done()
	return t.connect(ctx, network, address)
}

func (t *endpointTransport) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.http.CloseIdleConnections()
	t.active.Wait()
}
