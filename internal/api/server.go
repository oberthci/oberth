package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/runlog"
)

const maxRequestBytes = 1 << 20

type Actor struct {
	Identity    string `json:"identity"`
	Fingerprint string `json:"fingerprint"`
	Admin       bool   `json:"admin"`
}

type Authenticator interface {
	Authenticate(context.Context, string) (Actor, error)
}

type ToolService interface {
	CallTool(context.Context, Actor, string, json.RawMessage) (any, error)
}

type RunFilter struct {
	Repository string
	Ref        string
	Limit      int
}

type IssueFilter struct {
	Repository string
	Kind       string
	State      string
	BeforeID   int
	Limit      int
}

type ViewService interface {
	RunArtifacts(context.Context, Actor, string) (any, error)
	RunArtifact(context.Context, Actor, string, string, runlog.Filter) (any, error)
	Runs(context.Context, Actor, RunFilter) (any, error)
	Run(context.Context, Actor, string) (any, error)
	RunLog(context.Context, Actor, string, string, string) (any, error)
	RunLogFiltered(context.Context, Actor, string, string, string, runlog.Filter) (any, error)
	RunLogLive(context.Context, Actor, string, int64) (any, error)
	Repositories(context.Context, Actor) (any, error)
	Issues(context.Context, Actor, IssueFilter) (any, error)
	Status(context.Context, Actor) (any, error)
	Ready(context.Context) error
	// PublishRun force-syncs an already-green branch run to the upstream on
	// request. Only reachable when the server runs with --publish-on-green
	// =false; otherwise the run published itself and this is a no-op error.
	PublishRun(context.Context, Actor, string) error
}

// ErrorClassifier maps a service-layer error to an HTTP status code and a
// non-sensitive message. Injected at construction so the API package does not
// import service or store.
type ErrorClassifier func(error) (int, string)

type Server struct {
	auth          Authenticator
	tools         ToolService
	views         ViewService
	version       string
	classifyError ErrorClassifier
	mux           *http.ServeMux
}

type contextKey struct{}

func New(auth Authenticator, tools ToolService, views ViewService, version string, options ...func(*Server)) (*Server, error) {
	if auth == nil || tools == nil || views == nil {
		return nil, errors.New("authenticator, tool service, and view service are required")
	}
	if version == "" {
		version = "dev"
	}
	server := &Server{auth: auth, tools: tools, views: views, version: version, mux: http.NewServeMux()}
	for _, option := range options {
		option(server)
	}
	if server.classifyError == nil {
		server.classifyError = defaultClassifyError
	}
	server.routes()
	return server, nil
}

// WithErrorClassifier returns a server option that installs a custom error
// classifier for the dashboard view endpoints.
func WithErrorClassifier(classifier ErrorClassifier) func(*Server) {
	return func(server *Server) { server.classifyError = classifier }
}

// defaultClassifyError is the fallback when no classifier is injected. It
// returns a generic 500 for all errors.
func defaultClassifyError(error) (int, string) {
	return http.StatusInternalServerError, "internal error"
}

func (server *Server) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Strict-Transport-Security", "max-age=31536000")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		server.mux.ServeHTTP(writer, request)
	})
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	server.mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		if err := server.views.Ready(request.Context()); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
	})
	server.mux.Handle("POST /mcp", server.requireAuth(http.HandlerFunc(server.handleMCP)))
	server.mux.Handle("GET /api/runs", server.requireAuth(http.HandlerFunc(server.handleRuns)))
	server.mux.Handle("GET /api/runs/{run}", server.requireAuth(http.HandlerFunc(server.handleRunDetail)))
	server.mux.Handle("GET /api/runs/{run}/logs", server.requireAuth(http.HandlerFunc(server.handleRunLog)))
	server.mux.Handle("GET /api/runs/{run}/livelog", server.requireAuth(http.HandlerFunc(server.handleRunLogLive)))
	server.mux.Handle("GET /api/runs/{run}/artifacts", server.requireAuth(http.HandlerFunc(server.handleRunArtifacts)))
	server.mux.Handle("GET /api/runs/{run}/artifacts/{name...}", server.requireAuth(http.HandlerFunc(server.handleRunArtifactDownload)))
	server.mux.Handle("GET /api/repos", server.requireAuth(http.HandlerFunc(server.handleRepos)))
	server.mux.Handle("GET /api/issues", server.requireAuth(http.HandlerFunc(server.handleIssues)))
	server.mux.Handle("GET /api/status", server.requireAuth(http.HandlerFunc(server.handleStatus)))
	server.mux.Handle("POST /api/runs/{run}/publish", server.requireAuth(http.HandlerFunc(server.handleRunPublish)))
	server.mux.HandleFunc("GET /assets/{asset...}", serveAsset)
	server.mux.HandleFunc("GET /{$}", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/runs", http.StatusTemporaryRedirect)
	})
	server.mux.HandleFunc("GET /runs/{run}", func(writer http.ResponseWriter, _ *http.Request) { server.servePage(writer, "runs") })
	for _, path := range []string{"/runs", "/repos", "/issues", "/status"} {
		view := strings.TrimPrefix(path, "/")
		server.mux.HandleFunc("GET "+path, func(writer http.ResponseWriter, _ *http.Request) { server.servePage(writer, view) })
	}
}

func (server *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := bearerToken(request.Header.Values("Authorization"))
		if !ok {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="oberth"`)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		actor, err := server.auth.Authenticate(request.Context(), token)
		if err != nil || strings.TrimSpace(actor.Identity) == "" {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="oberth", error="invalid_token"`)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), contextKey{}, actor)))
	})
}

func bearerToken(headers []string) (string, bool) {
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(headers[0], "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\t\r\n ") {
		return "", false
	}
	return token, true
}

func actorFrom(ctx context.Context) Actor {
	actor, _ := ctx.Value(contextKey{}).(Actor)
	return actor
}

func (server *Server) handleRuns(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	filter := RunFilter{
		Repository: query.Get("repo"),
		Ref:        query.Get("ref"),
		Limit:      queryLimit(request, 100, 100),
	}
	value, err := server.views.Runs(request.Context(), actorFrom(request.Context()), filter)
	server.writeView(writer, value, err)
}

func (server *Server) handleRunDetail(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid run ID"})
		return
	}
	value, err := server.views.Run(request.Context(), actorFrom(request.Context()), id)
	server.writeView(writer, value, err)
}

const (
	maxLogPatternBytes = 512
	maxLogContextLines = 50
)

func (server *Server) handleRunLog(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	query := request.URL.Query()
	burn, step := query.Get("burn"), query.Get("step")
	if !validRunSelector(id) || !validRunSelector(burn) || !validRunSelector(step) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "run ID plus burn and step query parameters are required"})
		return
	}
	filter, err := logFilterFrom(query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	value, err := server.views.RunLogFiltered(request.Context(), actorFrom(request.Context()), id, burn, step, filter)
	server.writeView(writer, value, err)
}

func logFilterFrom(query url.Values) (runlog.Filter, error) {
	filter := runlog.Filter{Pattern: query.Get("pattern"), Tail: query.Get("tail") == "true"}
	for _, field := range []struct {
		name   string
		target *int
	}{{"context", &filter.Context}, {"limit", &filter.Limit}, {"offset", &filter.Offset}} {
		name, target := field.name, field.target
		raw := query.Get(name)
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return runlog.Filter{}, fmt.Errorf("invalid log %s: must be a non-negative integer", name)
		}
		*target = value
	}
	if len(filter.Pattern) > maxLogPatternBytes {
		return runlog.Filter{}, fmt.Errorf("log pattern exceeds %d bytes", maxLogPatternBytes)
	}
	if filter.Context > maxLogContextLines {
		return runlog.Filter{}, fmt.Errorf("log context exceeds %d lines", maxLogContextLines)
	}
	return filter, nil
}

// handleRunLogLive serves the polled live view of a running Job's redacted
// log stream. The offset query parameter is the caller's next-poll cursor; -1
// (the default) asks the store to start tailing near the current end.
// handleRunPublish publishes a green run to the upstream forge on request.
//
// POST rather than GET: it mutates the forge. It carries no body because the
// run ID is the whole request; anything else would be a second way to choose
// what gets pushed, and one is enough.
func (server *Server) handleRunPublish(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid run ID"})
		return
	}
	if err := server.views.PublishRun(request.Context(), actorFrom(request.Context()), id); err != nil {
		server.writeView(writer, nil, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "published", "run": id})
}

func (server *Server) handleRunLogLive(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid run ID"})
		return
	}
	offset := int64(-1)
	if raw := request.URL.Query().Get("offset"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < -1 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid log offset"})
			return
		}
		offset = value
	}
	value, err := server.views.RunLogLive(request.Context(), actorFrom(request.Context()), id, offset)
	server.writeView(writer, value, err)
}

// validRunSelector bounds dashboard path and query selectors before they reach
// storage: non-empty, trimmed, single-line, and shorter than any legitimate
// run ID, burn, or step name could ever be.
func validRunSelector(value string) bool {
	return value != "" && len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\t\r\n")
}

func (server *Server) handleRepos(writer http.ResponseWriter, request *http.Request) {
	value, err := server.views.Repositories(request.Context(), actorFrom(request.Context()))
	server.writeView(writer, value, err)
}

func (server *Server) handleIssues(writer http.ResponseWriter, request *http.Request) {
	filter, err := issueFilter(request)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid issue filter"})
		return
	}
	value, err := server.views.Issues(request.Context(), actorFrom(request.Context()), filter)
	server.writeView(writer, value, err)
}

func issueFilter(request *http.Request) (IssueFilter, error) {
	query := request.URL.Query()
	filter := IssueFilter{Repository: query.Get("repo"), Kind: query.Get("kind"), State: query.Get("state"), Limit: 50}
	if filter.Kind != "" && filter.Kind != "manual" && filter.Kind != "ci" {
		return IssueFilter{}, errors.New("invalid issue kind")
	}
	if filter.State != "" && filter.State != "open" && filter.State != "closed" {
		return IssueFilter{}, errors.New("invalid issue state")
	}
	if raw := query.Get("before"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return IssueFilter{}, errors.New("invalid issue cursor")
		}
		filter.BeforeID = value
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return IssueFilter{}, errors.New("invalid issue limit")
		}
		filter.Limit = min(value, 50)
	}
	return filter, nil
}

func (server *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	value, err := server.views.Status(request.Context(), actorFrom(request.Context()))
	server.writeView(writer, value, err)
}

func (server *Server) writeView(writer http.ResponseWriter, value any, err error) {
	if err != nil {
		code, message := server.classifyError(err)
		if code == http.StatusInternalServerError {
			log.Printf("api: view error: %v", err)
		}
		writeJSON(writer, code, map[string]string{"error": message})
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func queryLimit(request *http.Request, fallback, maximum int) int {
	value, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil || value < 1 {
		return fallback
	}
	return min(value, maximum)
}

func decodeOne(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON body must contain exactly one value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func strictUnmarshal(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return decodeOne(bytes.NewReader(raw), value)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// servePage writes the dashboard shell: server-rendered chrome (brand, tabs,
// status segments) plus the embedded stylesheet and script. All dynamic state
// is fetched by the script from the authenticated /api views with the
// browser-held bearer token, so this shell exposes no repository or run data.
func (server *Server) servePage(writer http.ResponseWriter, view string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_, _ = io.WriteString(writer, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Oberth Dashboard</title><link rel="stylesheet" href="/assets/app.css"></head><body data-version="`+html.EscapeString(server.version)+`"><div class="app"><header class="top"><button class="brand" data-route="/runs"><span class="mark">[▲]</span><span>oberth</span><span id="brandSub" class="sub">/ dashboard</span></button><nav class="tabs"><button id="tabRuns" class="tab" data-route="/runs">runs</button><button id="tabRepos" class="tab" data-route="/repos">repos</button><button id="tabIssues" class="tab" data-route="/issues">issues</button><button id="tabStatus" class="tab" data-route="/status">status</button></nav><div class="top-right"><div class="top-status" aria-label="System status"><span class="status-segment backend" title="loading" aria-label="loading"><span id="connLed" class="led" aria-hidden="true"></span><span id="connText">api</span></span><span class="status-divider" aria-hidden="true"></span><span id="versionStatus" class="status-segment version-mode unknown" role="status" aria-live="polite"><span>oberth</span> <span id="verLabel" class="ver">...</span></span></div><button class="iconbtn" data-action="toggle-theme" title="Theme" aria-label="Toggle theme"><svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg></button><button class="iconbtn" data-action="refresh" title="Refresh" aria-label="Refresh"><svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-2.6-6.4"/><path d="M21 3v6h-6"/></svg></button><button class="iconbtn" data-action="clear-token" title="Sign out" aria-label="Sign out"><svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/><path d="M10 17l5-5-5-5"/><path d="M15 12H3"/></svg></button></div></header><main id="app" class="main" data-view="`+view+`"><noscript><p>The Oberth dashboard requires JavaScript. The JSON views remain available at /api/runs, /api/repos, /api/issues, and /api/status with a bearer token.</p></noscript></main></div><dialog id="issueDialog" class="dlg" aria-labelledby="issueDialogTitle"><div class="dlg-shell"><header class="dlg-head"><h2 id="issueDialogTitle">Issue</h2><span id="issueDialogMeta" class="meta"></span><button class="dlg-close" data-action="close-issue" aria-label="Close issue details">×</button></header><div id="issueDialogContent" class="dlg-body"></div></div></dialog><script src="/assets/app.js" defer></script></body></html>`)
}

// serveAsset serves the embedded dashboard assets with explicit content types
// so nosniff never blocks them. The request name is only a key into the fixed
// startup-materialized asset map: unknown names are 404s and no request value
// ever reaches a filesystem read.
func serveAsset(writer http.ResponseWriter, request *http.Request) {
	asset, ok := staticAssets[request.PathValue("asset")]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", asset.contentType)
	writer.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = writer.Write(asset.body)
}
