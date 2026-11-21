package api

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config supplies transport dependencies. Source is returned only from the
// separate SourceHandler; it is never mounted in the user listener.
type Config struct {
	Auth          Authenticator
	Views         apicontract.Views
	Registry      *Registry
	Streams       Streams
	Enroll        apicontract.Handler[model.InviteRedemption, model.InviteCertificate]
	Runtime       Runtime
	Classify      Classifier
	Diagnostics   Diagnostics
	FailureLog    *slog.Logger
	Ready         func(context.Context) error
	Live          func(context.Context) error
	Metrics       func(context.Context, apicontract.Caller) (string, error)
	Source        http.Handler
	MaximumWait   time.Duration
	EventStreams  int
	ReplayBacklog int
}

type Server struct {
	observers       context.Context
	cancelObservers context.CancelFunc
	ui              uiBundle
	config          Config
	mux             *http.ServeMux
	draining        atomic.Bool
	drain           chan struct{}
	drainOnce       sync.Once
	sessions        sessions
	reservations    reservations
}

func New(c Config) (*Server, error) {
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	if c.MaximumWait == 0 {
		c.MaximumWait = 600 * time.Second
	}
	if c.EventStreams == 0 {
		c.EventStreams = eventsPerIdentity
	}
	if c.ReplayBacklog == 0 {
		c.ReplayBacklog = 500
	}

	registry := *c.Registry
	registry.maximumWait = c.MaximumWait
	c.Registry = &registry
	shutdown, cancelShutdown := context.WithCancel(context.Background())
	s := &Server{config: c, mux: http.NewServeMux(), drain: make(chan struct{}), observers: shutdown, cancelObservers: cancelShutdown}

	var err error
	s.ui, err = loadUI()
	if err != nil {
		cancelShutdown()
		return nil, err
	}
	s.sessions.items = map[string]*session{}
	s.reservations.counts = map[string]int{}
	s.routes()
	return s, nil
}
func (s *Server) Handler() http.Handler { return s.writeBound(http.HandlerFunc(s.serveUIOrAPI)) }
func (s *Server) SourceHandler() http.Handler {
	if s.config.Source == nil {
		return http.NotFoundHandler()
	}
	return s.writeBound(s.config.Source)
}

// BeginDrain withdraws readiness before refusing work and waking observers.
func (s *Server) BeginDrain() {
	s.draining.Store(true)
	s.drainOnce.Do(func() { close(s.drain); s.cancelObservers(); s.sessions.close() })
}

type endpoint func(http.ResponseWriter, *http.Request, apicontract.Caller)

func (s *Server) private(fn endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.draining.Load() {
			s.writeError(w, r, refusal("restarting"), 503)
			return
		}

		c, err := s.authenticate(r)
		if err != nil {
			status := 503
			if s.config.Classify(err).Code() == "forbidden" {
				status = 401
			}
			s.writeError(w, r, err, status)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/mcp") && !validProtocolHeader(r.Header) {
			s.writeError(w, r, refusal("method"), 400)
			return
		}
		fn(w, r, c)
	}
}
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, r, jsonReply{200, apicontract.SystemStatus{Ready: true}, apicontract.ResponseBytes})
	})
	s.mux.HandleFunc("GET /livez", s.probe(s.config.Live, false))
	s.mux.HandleFunc("GET /readyz", s.probe(s.config.Ready, true))
	s.mux.HandleFunc("POST /mcp", s.private(s.mcp))
	s.mux.HandleFunc("GET /mcp", s.private(s.eventStream))
	s.mux.HandleFunc("DELETE /mcp", s.private(s.deleteSession))
	s.mux.HandleFunc("POST /mcp/enroll", s.enrollment)
	s.mux.HandleFunc("POST /api/admin/{action}", s.private(s.admin))
	s.mux.HandleFunc("GET /metrics", s.private(s.metrics))
	s.mux.HandleFunc("GET /api/runs/{run}/containerlog/stream", s.private(s.nativeStream))
	s.readRoutes()
	s.uiRoutes()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.writeError(w, r, refusal("unknown"), 404) })
}
func (s *Server) probe(check func(context.Context) error, ready bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready && s.draining.Load() {
			s.writeError(w, r, refusal("restarting"), 503)
			return
		}
		if check != nil {
			if err := check(r.Context()); err != nil {
				s.writeError(w, r, err, 503)
				return
			}
		}
		s.writeJSON(w, r, jsonReply{200, apicontract.SystemStatus{Ready: true}, apicontract.ResponseBytes})
	}
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	if !apicontract.Metrics.Allows(c.Actor) {
		s.writeError(w, r, refusal("forbidden"), 403)
		return
	}
	if s.config.Metrics == nil {
		s.writeError(w, r, refusal("unknown"), 404)
		return
	}
	value, err := s.config.Metrics(r.Context(), c)
	if err != nil {
		s.writeError(w, r, err, 0)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(value))
}

func validateConfig(c Config) error {
	if nilHandler(c.Auth) || c.Registry == nil || c.Classify == nil || c.FailureLog == nil {
		return refusal("registry")
	}
	if err := validateRuntime(c.Runtime); err != nil {
		return err
	}
	if c.MaximumWait < 0 || c.MaximumWait > 600*time.Second {
		return refusal("input")
	}
	if c.EventStreams < 0 || c.EventStreams > 64 || c.ReplayBacklog < 0 || c.ReplayBacklog > 10000 {
		return refusal("input")
	}
	return nil
}

func validateRuntime(r Runtime) error {
	if r.Now == nil || r.Nonce == nil || r.Ticker == nil {
		return refusal("registry")
	}
	return nil
}

// WriteTimeout gives the composition root the ceiling derived from the registry.
func (s *Server) WriteTimeout() time.Duration {
	maximum := s.config.MaximumWait
	for _, tool := range s.config.Registry.entries {
		maximum = max(maximum, tool.meta.Deadline)
	}
	return maximum + time.Minute
}
