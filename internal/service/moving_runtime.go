package service

import (
	"context"
	"time"
)

type gate struct {
	permit chan struct{}
	users  int
}

func (s *Service) acquire(ctx context.Context, key string) (func(), error) {
	s.mu.Lock()
	g := s.gates[key]
	if g == nil {
		g = &gate{permit: make(chan struct{}, 1)}
		s.gates[key] = g
	}
	g.users++
	s.mu.Unlock()
	select {
	case g.permit <- struct{}{}:
		return func() { <-g.permit; s.dropGate(key, g) }, nil
	case <-ctx.Done():
		s.dropGate(key, g)
		return nil, ctx.Err()
	}
}
func (s *Service) dropGate(key string, g *gate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g.users--
	if g.users == 0 {
		delete(s.gates, key)
	}
}
func refKey(repo, ref string) string { return "ref:" + repo + "\x00" + ref }

func (s *Service) schedule(id string, at *time.Time) {
	delay := time.Duration(0)
	if at != nil {
		delay = max(0, at.Sub(s.c.Now()))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	if old := s.timers[id]; old != nil {
		old.Stop()
	}
	s.timers[id] = s.c.AfterFunc(delay, func() { s.deliverTask(id) })
}
func (s *Service) deliverTask(id string) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	delete(s.timers, id)
	s.tasks.Add(1)
	s.mu.Unlock()
	defer s.tasks.Done()
	if err := s.Deliver(s.ctx, id); err != nil {
		s.c.OnError(err)
	}
}
func (s *Service) stopMoving(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.cancel()
	for _, timer := range s.timers {
		timer.Stop()
	}
	s.timers = map[string]Timer{}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.tasks.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
