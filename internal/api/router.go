package api

import "net/http"

// routeMux retains the registration inventory used by access-control tests.
// Every registration, including public handlers, goes through this wrapper.
type routeMux struct {
	mux      *http.ServeMux
	patterns []string
}

func newRouteMux() *routeMux { return &routeMux{mux: http.NewServeMux()} }
func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.mux.Handle(pattern, h)
	m.patterns = append(m.patterns, pattern)
}
func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}
func (m *routeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }
