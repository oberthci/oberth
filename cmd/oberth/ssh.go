package main

import (
	"context"
	"errors"
	"net"
	"sync"
)

type sshServing struct {
	mu       sync.Mutex
	listener net.Listener
	stopped  bool
	bound    chan net.Listener
}

func sshPart(c *composition, listen func(context.Context, string, string) (net.Listener, error)) Part {
	s := &sshServing{bound: make(chan net.Listener, 1)}
	return Part{Name: "ssh", Run: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case listener := <-s.bound:
			err := c.ssh(ctx, listener)
			if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
	}, Recover: func(ctx context.Context) error {
		listener, err := listen(ctx, "tcp", c.config.Listen.SSH)
		if err != nil {
			return serverProblem("config", "listen.ssh "+c.config.Listen.SSH+": cannot bind", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stopped {
			_ = listener.Close()
			return context.Canceled
		}
		s.listener = listener
		s.bound <- listener
		return nil
	}, Stop: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stopped = true
		if s.listener != nil {
			_ = s.listener.Close()
		}
	}}
}
