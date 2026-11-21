package sshtest

import (
	"net"
	"sync"
)

// Listener hands one in-memory connection to a native server and blocks further
// accepts until Close, just like a quiet local listener.
func Listener(conn net.Conn) net.Listener { return &listener{conn: conn, closed: make(chan struct{})} }

type listener struct {
	conn     net.Conn
	mu       sync.Mutex
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (p *listener) Accept() (net.Conn, error) {
	p.mu.Lock()
	if !p.accepted {
		p.accepted = true
		p.mu.Unlock()
		return p.conn, nil
	}
	p.mu.Unlock()
	<-p.closed
	return nil, net.ErrClosed
}
func (p *listener) Close() error   { p.once.Do(func() { close(p.closed) }); return nil }
func (p *listener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }
