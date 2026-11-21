// Package sshtest supplies joined in-memory connections for native SSH contracts.
//
// Spec: docs/plans/c6-source-delivery.md#inspections
package sshtest

import (
	"bytes"
	"io"
	"net"
	"sync"
)

// Pipe buffers SSH's symmetric version writes without opening a network socket.
func Pipe() (net.Conn, net.Conn) { left, right := net.Pipe(); return buffered(left), buffered(right) }

type connection struct {
	net.Conn
	out        chan packet
	done, stop chan struct{}
	once       sync.Once
}

func buffered(c net.Conn) *connection {
	a := &connection{Conn: c, out: make(chan packet, 64), done: make(chan struct{}), stop: make(chan struct{})}
	go func() {
		defer close(a.done)
		for {
			select {
			case <-a.stop:
				return
			case b := <-a.out:
				if len(b.data) > 0 {
					if _, err := c.Write(b.data); err != nil {
						return
					}
				}
				if b.done != nil {
					close(b.done)
				}
			}
		}
	}()
	return a
}
func (a *connection) Write(b []byte) (int, error) {
	select {
	case a.out <- packet{data: bytes.Clone(b)}:
		return len(b), nil
	case <-a.done:
		return 0, io.ErrClosedPipe
	}
}
func (a *connection) Close() error {
	err := a.Conn.Close()
	a.once.Do(func() { close(a.stop) })
	<-a.done
	return err
}

func (a *connection) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
}
func (a *connection) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
}

type packet struct {
	data []byte
	done chan struct{}
}

// Flush waits for queued writes at a deterministic protocol hand-off.
func Flush(conn net.Conn) error {
	c := conn.(*connection)
	done := make(chan struct{})
	select {
	case c.out <- packet{done: done}:
	case <-c.done:
		return io.ErrClosedPipe
	}
	select {
	case <-done:
		return nil
	case <-c.done:
		return io.ErrClosedPipe
	}
}
