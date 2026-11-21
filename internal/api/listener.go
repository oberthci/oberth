package api

import (
	"net/http"
	"time"
)

const (
	ListenerConnections   = 512
	headerReadTimeout     = 10 * time.Second
	bodyReadTimeout       = time.Minute
	connectionIdleTimeout = 2 * time.Minute
	startupWriteTimeout   = time.Minute
	eventWriteTimeout     = 5 * time.Second
	logWriteTimeout       = 30 * time.Second
)

// ListenerServer supplies the standard HTTP bounds for both HTTPS listeners.
// API and source routes replace the startup write bound with Server.WriteTimeout.
func ListenerServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: headerReadTimeout, ReadTimeout: bodyReadTimeout, IdleTimeout: connectionIdleTimeout, WriteTimeout: startupWriteTimeout}
}

func (s *Server) writeBound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(s.config.Runtime.Now().Add(s.WriteTimeout()))
		next.ServeHTTP(w, r)
	})
}
