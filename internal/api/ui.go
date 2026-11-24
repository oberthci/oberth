package api

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"strings"
)

//go:embed ui
var uiFiles embed.FS

const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; require-trusted-types-for 'script'"

type uiAsset struct {
	body          []byte
	kind, version string
}
type uiBundle struct {
	shell  []byte
	assets map[string]uiAsset
}

func (s *Server) serveUIOrAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		if _, ok := s.ui.assets[strings.TrimPrefix(r.URL.Path, "/assets/")]; !ok {
			http.NotFound(w, r)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func loadUI() (uiBundle, error) {
	b := uiBundle{assets: make(map[string]uiAsset)}
	for name, kind := range map[string]string{"app.js": "text/javascript; charset=utf-8", "status.js": "text/javascript; charset=utf-8", "status.css": "text/css; charset=utf-8", "log.js": "text/javascript; charset=utf-8", "issues.js": "text/javascript; charset=utf-8", "run.js": "text/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8"} {
		body, err := uiFiles.ReadFile("ui/" + name)
		if err != nil {
			return b, err
		}
		sum := sha256.Sum256(body)
		b.assets[name] = uiAsset{body, kind, fmt.Sprintf("%x", sum[:8])}
	}
	shell, err := uiFiles.ReadFile("ui/index.html")
	if err != nil {
		return b, err
	}
	page := string(shell)
	for name, asset := range b.assets {
		page = strings.ReplaceAll(page, "/assets/"+name, "/assets/"+name+"?v="+asset.version)
	}
	b.shell = []byte(page)
	return b, nil
}

func (s *Server) uiRoutes() {
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/runs", http.StatusSeeOther) })
	for _, path := range []string{"/runs", "/runs/{run}", "/repos", "/issues", "/issues/{id}", "/status"} {
		s.mux.HandleFunc("GET "+path, s.uiShell)
	}
	s.mux.HandleFunc("GET /assets/{name}", s.uiAsset)
}
func (s *Server) uiShell(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.ui.shell)
}
func (s *Server) uiAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.ui.assets[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	cache := "no-cache"
	if r.URL.Query().Get("v") == asset.version {
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Type", asset.kind)
	_, _ = w.Write(asset.body)
}
