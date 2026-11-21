package sshserver

import (
	"golang.org/x/crypto/ssh"
	"io"
	"sync"
	"time"
)

type idleChannel struct {
	ssh.Channel
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	active   bool
	expire   func()
	now      func() time.Time
	deadline time.Time
}

func newIdle(ch ssh.Channel, timeout time.Duration, expire func(), now func() time.Time) *idleChannel {
	return &idleChannel{Channel: ch, timeout: timeout, expire: expire, now: now}
}
func (c *idleChannel) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.deadline = c.now().Add(c.timeout)
	c.timer = time.AfterFunc(c.timeout, c.fire)
}
func (c *idleChannel) fire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	if remaining := c.deadline.Sub(c.now()); remaining > 0 {
		c.timer.Reset(remaining)
		return
	}
	c.active = false
	c.expire()
}
func (c *idleChannel) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	if c.timer != nil {
		c.timer.Stop()
	}
}
func (c *idleChannel) close() { c.end() }
func (c *idleChannel) progress(n int) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active {
		c.deadline = c.now().Add(c.timeout)
		c.timer.Reset(c.timeout)
	}
}
func (c *idleChannel) Read(b []byte) (int, error) {
	n, e := c.Channel.Read(b)
	c.progress(n)
	return n, e
}
func (c *idleChannel) Write(b []byte) (int, error) {
	n, e := c.Channel.Write(b)
	c.progress(n)
	return n, e
}
func (c *idleChannel) stderr() io.Writer {
	return progressWriter{writer: c.Stderr(), progress: c.progress}
}

type progressWriter struct {
	writer   io.Writer
	progress func(int)
}

func (w progressWriter) Write(b []byte) (int, error) {
	n, e := w.writer.Write(b)
	w.progress(n)
	return n, e
}
