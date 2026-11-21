package installer_test

import (
	"bytes"
	"net"
	"sync"
)

// queuedTLS lets an alert cross a pending handshake write on a net.Pipe pair.
// The queue is bounded; closing either side releases the writer goroutine.
type queuedTLS struct {
	net.Conn
	writes     chan []byte
	stop, done chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

func bufferTLS(conn net.Conn) *queuedTLS {
	p := &queuedTLS{Conn: conn, writes: make(chan []byte, 8), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			select {
			case data, ok := <-p.writes:
				if !ok {
					return
				}
				if _, err := conn.Write(data); err != nil {
					return
				}
			case <-p.stop:
				return
			}
		}
	}()
	return p
}
func (p *queuedTLS) Write(data []byte) (int, error) {
	select {
	case p.writes <- bytes.Clone(data):
		return len(data), nil
	case <-p.done:
		return 0, net.ErrClosed
	}
}
func (p *queuedTLS) finish() { close(p.writes); <-p.done }
func (p *queuedTLS) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.Conn.Close()
		close(p.stop)
		<-p.done
	})
	return p.closeErr
}
