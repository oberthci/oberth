package sshserver

import (
	"golang.org/x/crypto/ssh"
	"io"
	"sync"
	"time"
)

// Stream progress, rather than SSH keepalives or traffic on another channel,
// renews this deadline. The absolute connection lifetime remains in force.
type idleChannel struct {
	ssh.Channel
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	stopped  bool
	expire   func()
}

func newIdleChannel(channel ssh.Channel, timeout time.Duration, expire func()) *idleChannel {
	return &idleChannel{Channel: channel, timeout: timeout, expire: expire}
}

// BeginTransport arms inactivity detection only while the Git subprocess owns I/O.
func (c *idleChannel) BeginTransport() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timeout > 0 && !c.stopped {
		c.deadline = time.Now().Add(c.timeout)
		c.timer = time.AfterFunc(c.timeout, c.check)
	}
}

// EndTransport leaves finalization and receive replay under the session lifetime.
func (c *idleChannel) EndTransport() { c.stop() }
func (c *idleChannel) check() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if remaining := time.Until(c.deadline); remaining > 0 {
		c.timer.Reset(remaining)
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()
	c.expire()
}
func (c *idleChannel) progress(n int) {
	if n > 0 {
		c.mu.Lock()
		if !c.stopped {
			c.deadline = time.Now().Add(c.timeout)
		}
		c.mu.Unlock()
	}
}
func (c *idleChannel) stop() {
	c.mu.Lock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
}
func (c *idleChannel) Read(p []byte) (int, error) {
	n, err := c.Channel.Read(p)
	c.progress(n)
	return n, err
}
func (c *idleChannel) Write(p []byte) (int, error) {
	n, err := c.Channel.Write(p)
	c.progress(n)
	return n, err
}
func (c *idleChannel) Stderr() io.ReadWriter {
	return idleExtendedData{channel: c, stream: c.Channel.Stderr()}
}

type idleExtendedData struct {
	channel *idleChannel
	stream  io.ReadWriter
}

func (s idleExtendedData) Read(p []byte) (int, error) {
	n, err := s.stream.Read(p)
	s.channel.progress(n)
	return n, err
}
func (s idleExtendedData) Write(p []byte) (int, error) {
	n, err := s.stream.Write(p)
	s.channel.progress(n)
	return n, err
}
