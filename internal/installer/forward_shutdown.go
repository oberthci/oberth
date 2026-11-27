package installer

import (
	"errors"
	"net"
	"net/http"
	"syscall"

	"k8s.io/apimachinery/pkg/util/httpstream"
)

// A closed forwarding peer is an ordinary disconnect. Normalize its write
// error so the native forwarder handles it as closure without a global log.
type forwardingDialer struct{ httpstream.Dialer }

func (d forwardingDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	conn, protocol, err := d.Dialer.Dial(protocols...)
	if err != nil {
		return nil, protocol, err
	}
	return forwardingConnection{conn}, protocol, nil
}

type forwardingConnection struct{ httpstream.Connection }

func (c forwardingConnection) CreateStream(headers http.Header) (httpstream.Stream, error) {
	stream, err := c.Connection.CreateStream(headers)
	if err != nil {
		return nil, err
	}
	return forwardingStream{stream}, nil
}

type forwardingStream struct{ httpstream.Stream }

func (s forwardingStream) Write(data []byte) (int, error) {
	n, err := s.Stream.Write(data)
	if errors.Is(err, syscall.EPIPE) {
		err = net.ErrClosed
	}
	return n, err
}
