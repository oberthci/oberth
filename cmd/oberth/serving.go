package main

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/oberthci/oberth/internal/api"
	"golang.org/x/net/netutil"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

type httpRoutes struct{ api, source http.Handler }
type serving struct {
	c              *composition
	life           *lifecycle
	deps           serverDependencies
	servers        []*http.Server
	listeners      []net.Listener
	workers        sync.WaitGroup
	failures       chan error
	starting       chan error
	routes         atomic.Pointer[httpRoutes]
	requestCtx     context.Context
	cancelRequests context.CancelFunc
}

func newServing(c *composition, life *lifecycle, deps serverDependencies) *serving {
	s := &serving{c: c, life: life, deps: deps, failures: make(chan error, 3)}
	s.requestCtx, s.cancelRequests = context.WithCancel(life.ctx)
	for i, address := range []string{c.config.Listen.HTTPS, c.config.Listen.Source, c.config.Listen.Health} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			routes := s.routes.Load()
			if i == 0 {
				routes.api.ServeHTTP(w, r)
			} else {
				routes.source.ServeHTTP(w, r)
			}
		})
		server := api.ListenerServer(c.health.gate(handler))
		if i == 2 {
			server = probeServer(http.HandlerFunc(c.health.probe))
		}
		server.ErrorLog = log.New(listenerLog{c.health, c.diagnostic}, "", log.LstdFlags)
		server.Addr = address
		server.BaseContext = func(net.Listener) context.Context { return s.requestCtx }
		s.servers = append(s.servers, server)
	}
	return s
}
func (s *serving) publish() { s.routes.Store(&httpRoutes{s.c.api.Handler(), s.c.api.SourceHandler()}) }
func (s *serving) listen(ctx context.Context, identity *serverIdentity) error {
	for i, server := range s.servers {
		listener, err := s.deps.listen(ctx, "tcp", server.Addr)
		if err != nil {
			key := []string{"listen.https", "listen.source", "listen.health"}[i]
			return serverProblem("config", key+" "+server.Addr+": cannot bind", err)
		}
		limit := api.ListenerConnections
		if i == 2 {
			limit = probeConnections
		}
		listener = netutil.LimitListener(listener, limit)
		s.listeners = append(s.listeners, listener)
		if i != 2 {
			listener = tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: identity.configuration})
		}
		s.workers.Go(func() {
			err := server.Serve(listener)
			if !errors.Is(err, http.ErrServerClosed) {
				s.failures <- err
			}
		})
	}
	return nil
}
func (s *serving) start(ctx context.Context) error {
	select {
	case err := <-s.starting:
		s.starting = nil
		if cancellationOnly(err) && ctx.Err() != nil {
			return nil
		}
		return err
	case err := <-s.failures:
		return err
	case <-ctx.Done():
		return nil
	}
}
func (s *serving) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.life.failures:
		return err
	case err := <-s.failures:
		return err
	}
}
