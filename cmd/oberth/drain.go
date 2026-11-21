package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

func (s *serving) close(parent context.Context) error {
	ctx, cancel := s.deps.timeout(context.WithoutCancel(parent), 25*time.Second)
	defer cancel()
	s.life.abortStart()
	s.c.health.stop()
	if s.c.api != nil {
		s.c.api.BeginDrain()
	}
	httpCtx, stopHTTP := s.deps.timeout(ctx, 15*time.Second)
	result := drainHTTP(httpCtx, s.servers)
	stopHTTP()
	for _, listener := range s.listeners {
		_ = listener.Close()
	}
	s.cancelRequests()
	s.life.stopListeners()
	joined, startupErr := s.joinStartup(ctx)
	if result == nil && joined {
		result = errors.Join(startupErr, s.life.stop(ctx))
	} else {
		s.life.cancel()
		if result != nil {
			result = serverProblem("shutdown", "Part: HTTP requests", result)
		}
		result = errors.Join(result, startupErr)
	}
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		result = errors.Join(result, serverProblem("shutdown", "Part: HTTP listener", ctx.Err()))
	}
	return result
}
func (s *serving) joinStartup(ctx context.Context) (bool, error) {
	if s.starting == nil {
		return true, nil
	}
	select {
	case err := <-s.starting:
		s.starting = nil
		if cancellationOnly(err) {
			return true, nil
		}
		return true, err
	case <-ctx.Done():
		return false, serverProblem("shutdown", "Part: startup", ctx.Err())
	}
}
func drainHTTP(ctx context.Context, servers []*http.Server) error {
	out := make(chan error, len(servers))
	var group sync.WaitGroup
	for _, server := range servers {
		group.Go(func() {
			err := server.Shutdown(ctx)
			if err != nil {
				err = errors.Join(err, server.Close())
			}
			out <- err
		})
	}
	group.Wait()
	close(out)
	var result error
	for err := range out {
		result = errors.Join(result, err)
	}
	return result
}
