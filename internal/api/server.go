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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

const maxRequestBytes = 1 << 20

type Actor struct {
	Identity    string `json:"identity"`
	Fingerprint string `json:"fingerprint"`
	Admin       bool   `json:"admin"`
	// Scope limits the uplink to the repositories of these upstreams, by
	// name. Empty sees every repository (#073).
	Scope []string `json:"scope,omitempty"`
}

// Scoped reports whether the actor is limited to some upstreams.
func (actor Actor) Scoped() bool { return len(actor.Scope) > 0 }

// SeesUpstream reports whether the actor may see repositories of upstream.
func (actor Actor) SeesUpstream(upstream string) bool {
	return model.ScopeCovers(actor.Scope, upstream)
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
	// Before pages to runs older than this QueueSequence (exclusive).
	Before int64
	// BeforeRun pages to runs older than this run, by ID: the cursor a
	// scoped uplink gets instead of a global sequence (#073).
	BeforeRun string
	// View selects the list: "" or "all" puts the queue first, followed by
	// dated runs; "history" keeps the old terminal-only paging; "active" is
	// the unpaged compatibility view of non-terminal runs.
	View string
}

// Run list views accepted by RunFilter.View.
const (
	RunViewAll     = "all"
	RunViewActive  = "active"
	RunViewHistory = "history"
)

// ValidRunView reports whether view names a run list view; empty is "all".
func ValidRunView(view string) bool {
	switch view {
	case "", RunViewAll, RunViewActive, RunViewHistory:
		return true
	}
	return false
}

type IssueFilter struct {
	Repository string
	Kind       string
	State      string
	Query      string
	BeforeID   int
	Limit      int
}

// GrantFilter constrains the read-only grants view.
type GrantFilter struct {
	Repository string
	SHA        string
	Status     string
}

type ViewService interface {
	RunArtifacts(context.Context, Actor, string) (any, error)
	RunArtifact(context.Context, Actor, string, string, runlog.Filter) (any, error)
	Runs(context.Context, Actor, RunFilter) (any, error)
	RunCounts(context.Context, Actor) (any, error)
	Run(context.Context, Actor, string) (any, error)
	RunLog(context.Context, Actor, string, string, string) (any, error)
	RunLogFiltered(context.Context, Actor, string, string, string, runlog.Filter) (any, error)
	RunLogLive(context.Context, Actor, string, int64) (any, error)
	Repositories(context.Context, Actor) (any, error)
	Issues(context.Context, Actor, IssueFilter) (any, error)
	Grants(context.Context, Actor, GrantFilter) (any, error)
	Status(context.Context, Actor) (any, error)
	Ready(context.Context) error
}

// ErrorClassifier maps a service-layer error to an HTTP status code and a
// non-sensitive message. Injected at construction so the API package does not
// import service or store.
type ErrorClassifier func(error) (int, string)

// writeDeadlineMargin is added to MaximumToolWait to derive the HTTP
// write-deadline extension for long-poll tools. It must exceed the maximum
// wait ceiling so the HTTP timeout never terminates a response that the
// service layer would still return.
const writeDeadlineMargin = 1 * time.Minute

type Server struct {
	draining              atomic.Bool
	drainContext          context.Context
	stopObservers         context.CancelFunc
	mcpWaitLog            func(mcpWaitEvent) // Private fixed-schema sink; nil uses the process logger.
	auth                  Authenticator
	tools                 ToolService
	views                 ViewService
	downloads             ArtifactDownloads
	version               string
	classifyError         ErrorClassifier
	maximumToolWait       time.Duration
	mcpHeartbeatInterval  time.Duration
	mcpStreamWriteTimeout time.Duration
	mux                   *http.ServeMux
}

type contextKey struct{}

func New(auth Authenticator, tools ToolService, views ViewService, version string, options ...func(*Server)) (*Server, error) {
	if auth == nil || tools == nil || views == nil {
		return nil, errors.New("authenticator, tool service, and view service are required")
	}
	// The raw download route must never fall back to the bounded text view;
	// that fallback was the silent 4 MiB truncation (#1011).
	downloads, ok := views.(ArtifactDownloads)
	if !ok {
		return nil, errors.New("view service must provide raw artifact downloads")
	}
	if version == "" {
		version = "dev"
	}
	drainContext, stopObservers := context.WithCancel(context.Background())
	server := &Server{drainContext: drainContext, stopObservers: stopObservers, auth: auth, tools: tools, views: views, downloads: downloads, version: version, mux: http.NewServeMux()}
	for _, option := range options {
		option(server)
	}
	if server.classifyError == nil {
		server.classifyError = defaultClassifyError
	}
	if server.maximumToolWait <= 0 {
		server.maximumToolWait = 10 * time.Minute
	}
	server.routes()
	return server, nil
}

// WithErrorClassifier returns a server option that installs a custom error
// classifier for the dashboard view endpoints.
func WithErrorClassifier(classifier ErrorClassifier) func(*Server) {
	return func(server *Server) { server.classifyError = classifier }
}

// WithMaximumToolWait sets the service-layer wait ceiling so the HTTP
// write-deadline extension for long-poll tools can be derived from it
// rather than a hand-maintained literal (#793).
func WithMaximumToolWait(d time.Duration) func(*Server) {
	return func(server *Server) { server.maximumToolWait = d }
}

// defaultClassifyError is the fallback when no classifier is injected. It
// returns a generic 500 for all errors.
func defaultClassifyError(error) (int, string) {
	return http.StatusInternalServerError, "internal error"
}

// BeginDrain first removes readiness, then stops read-only long-poll observers.
// Accepted mutations retain their request contexts and may finish during Shutdown.
func (server *Server) BeginDrain() {
	server.draining.Store(true)
	server.stopObservers()
}

// RestartingRPCCode tells observers to reconnect without treating a shutdown as
// a terminal run result. Mutating tools must never be retried on this signal.
const RestartingRPCCode = -32001

func restartingRPC() *rpcError {
	return &rpcError{Code: RestartingRPCCode, Message: "server restarting; reconnect to observe durable work"}
}

func (server *Server) refuseDuringDrain(writer http.ResponseWriter) bool {
	if !server.draining.Load() {
		return false
	}
	writer.Header().Set("Retry-After", "1")
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "server restarting"})
	return true
}

func (server *Server) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Strict-Transport-Security", "max-age=31536000")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if request.URL.Path != "/healthz" && server.refuseDuringDrain(writer) {
			return
		}
		if request.Method == http.MethodGet {
			ctx, cancel := context.WithCancel(request.Context())
			stop := context.AfterFunc(server.drainContext, cancel)
			defer stop()
			defer cancel()
			request = request.WithContext(ctx)
		}
		server.mux.ServeHTTP(writer, request)
	})
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	server.mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		if err := server.views.Ready(request.Context()); err != nil || server.draining.Load() {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
	})
	server.mux.Handle("POST /mcp", server.requireAuth(http.HandlerFunc(server.handleMCP)))
	server.mux.Handle("GET /api/runs", server.requireAuth(http.HandlerFunc(server.handleRuns)))
	server.mux.Handle("GET /api/runs/counts", server.requireAuth(http.HandlerFunc(server.handleRunCounts)))
	server.mux.Handle("GET /api/runs/{run}", server.requireAuth(http.HandlerFunc(server.handleRunDetail)))
	server.mux.Handle("GET /api/runs/{run}/logs", server.requireAuth(http.HandlerFunc(server.handleRunLog)))
	server.mux.Handle("GET /api/runs/{run}/livelog/stream", server.requireAuth(http.HandlerFunc(server.handleRunLogLiveStream)))
	server.mux.Handle("GET /api/runs/{run}/livelog", server.requireAuth(http.HandlerFunc(server.handleRunLogLive)))
	server.mux.Handle("GET /api/runs/{run}/artifacts", server.requireAuth(http.HandlerFunc(server.handleRunArtifacts)))
	server.mux.Handle("GET /api/runs/{run}/artifacts/{name...}", server.requireAuth(http.HandlerFunc(server.handleRunArtifactDownload)))
	server.mux.Handle("GET /api/repos", server.requireAuth(http.HandlerFunc(server.handleRepos)))
	server.mux.Handle("GET /api/issues", server.requireAuth(http.HandlerFunc(server.handleIssues)))
	server.mux.Handle("GET /api/grants", server.requireAuth(http.HandlerFunc(server.handleGrants)))
	server.mux.Handle("GET /api/audit", server.requireAuth(http.HandlerFunc(server.handleAudit)))
	server.mux.Handle("GET /api/schedules", server.requireAuth(http.HandlerFunc(server.handleSchedules)))
	server.mux.Handle("GET /api/status", server.requireAuth(http.HandlerFunc(server.handleStatus)))
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
		if server.refuseDuringDrain(writer) {
			return
		}
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
		View:       query.Get("view"),
	}
	// ?view=history is terminal runs only, paged like the default list;
	// ?view=active is the unpaged non-terminal list in scheduling order.
	// Without view the body and its paging are unchanged.
	if !ValidRunView(filter.View) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "view must be all, active or history"})
		return
	}
	if raw := query.Get("before"); raw != "" {
		before, parseErr := strconv.ParseInt(raw, 10, 64)
		switch {
		case parseErr == nil && before >= 1:
			filter.Before = before
			// A run ID (or its 12+ character prefix) can be all digits; the
			// service tries it as a run first and falls back to the
			// sequence only for an unscoped caller (#073).
			if len(raw) >= 12 {
				filter.BeforeRun = raw
			}
		case parseErr != nil && validRunSelector(raw):
			filter.BeforeRun = raw
		default:
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "before must be a positive run sequence or a run ID"})
			return
		}
	}
	value, err := server.views.Runs(request.Context(), actorFrom(request.Context()), filter)
	// The body remains a plain array. The default first page also carries
	// the whole bounded queue, so only its admitted/finished portion counts
	// against the page limit.
	if runs, ok := value.([]model.Run); ok && err == nil && RunPageFull(runs, filter) {
		last := runs[len(runs)-1]
		cursor := last.ID
		if last.QueueSequence > 0 {
			cursor = strconv.FormatInt(last.QueueSequence, 10)
		}
		writer.Header().Set("X-Oberth-Next-Before", cursor)
	}
	server.writeView(writer, value, err)
}

// RunPageFull reports whether the time-ordered part of a run page is full.
func RunPageFull(runs []model.Run, filter RunFilter) bool {
	if len(runs) == 0 || filter.View == RunViewActive {
		return false
	}
	if filter.View == RunViewHistory {
		return len(runs) == filter.Limit
	}
	count := 0
	for _, run := range runs {
		if run.StartedAt != nil || run.FinishedAt != nil || run.Status.Terminal() {
			count++
		}
	}
	return count == filter.Limit
}

func (server *Server) handleRunCounts(writer http.ResponseWriter, request *http.Request) {
	value, err := server.views.RunCounts(request.Context(), actorFrom(request.Context()))
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
func (server *Server) handleRunLogLive(writer http.ResponseWriter, request *http.Request) {
	id, offset, ok := liveLogCursor(writer, request)
	if !ok {
		return
	}
	value, err := server.views.RunLogLive(request.Context(), actorFrom(request.Context()), id, offset)
	server.writeView(writer, value, err)
}

func liveLogCursor(writer http.ResponseWriter, request *http.Request) (string, int64, bool) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid run ID"})
		return "", 0, false
	}
	offset := int64(-1)
	if raw := request.URL.Query().Get("offset"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < -1 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid log offset"})
			return "", 0, false
		}
		offset = value
	}
	return id, offset, true
}

// LiveLogResponse is a bounded slice of the redacted run log. Done is true
// only after a terminal run's final bytes have been read.
type LiveLogResponse struct {
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	DisplayStatus string `json:"display_status,omitempty"`
	DisplayReason string `json:"display_reason"`
	Chunk         string `json:"chunk"`
	Offset        int64  `json:"offset"`
	Size          int64  `json:"size"`
	Done          bool   `json:"done"`
}

const liveLogAuthInterval = 30 * time.Second

func (server *Server) handleRunLogLiveStream(writer http.ResponseWriter, request *http.Request) {
	id, offset, ok := liveLogCursor(writer, request)
	if !ok {
		return
	}
	// Use the full authorized view initially, on log changes and roughly once
	// a second so terminal state and evidence/scope changes remain visible.
	read := func() (LiveLogResponse, error) {
		value, err := server.views.RunLogLive(request.Context(), actorFrom(request.Context()), id, offset)
		if err != nil {
			return LiveLogResponse{}, err
		}
		chunk, ok := value.(LiveLogResponse)
		if !ok {
			return LiveLogResponse{}, errors.New("invalid live log response")
		}
		return chunk, nil
	}
	chunk, err := read()
	lastRead := time.Now()
	lastAuth := lastRead
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	if !supportsMCPFlush(writer) {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "streaming unavailable"})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(writer)
	write := func(frame string) error {
		// Refresh the server-wide write deadline for each frame, bounding a
		// slow reader without imposing a hard lifetime on healthy streams.
		if err := controller.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := io.WriteString(writer, frame); err != nil {
			return err
		}
		return controller.Flush()
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	if err := write(": ping\n\n"); err != nil {
		return
	}
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		if request.Context().Err() != nil {
			return
		}
		offset = chunk.Offset
		if chunk.Chunk != "" || chunk.Done {
			body, err := json.Marshal(chunk)
			if err != nil || write("event: chunk\ndata: "+string(body)+"\n\n") != nil {
				return
			}
		}
		if chunk.Done {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-heartbeat.C:
			if err := write(": ping\n\n"); err != nil {
				return
			}
		case <-tick.C:
		}
		if time.Since(lastAuth) >= liveLogAuthInterval {
			token, ok := bearerToken(request.Header.Values("Authorization"))
			authCtx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
			fresh, authErr := server.auth.Authenticate(authCtx, token)
			cancel()
			original := actorFrom(request.Context())
			if !ok || authErr != nil || strings.TrimSpace(fresh.Identity) == "" || fresh.Identity != original.Identity || fresh.Fingerprint != original.Fingerprint || fresh.Admin != original.Admin || !slices.Equal(fresh.Scope, original.Scope) {
				_ = write("event: auth_error\ndata: {\"error\":\"stream authorization changed or expired; reconnect with a current bearer token\"}\n\n")
				return
			}
			lastAuth = time.Now()
		}
		if source, ok := server.views.(interface{ LiveLogSize(string) (int64, error) }); ok && time.Since(lastRead) < time.Second {
			// The first view resolved any prefix and authorized this canonical ID.
			// Stat failures fall through to the normal view's error handling.
			if size, err := source.LiveLogSize(chunk.RunID); err == nil && size == offset {
				chunk.Chunk = ""
				continue
			}
		}
		chunk, err = read()
		lastRead = time.Now()
		if err != nil {
			// Closing makes clients resume through the authenticated poll
			// endpoint, which can report the normal structured view error.
			return
		}
	}
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
	filter := IssueFilter{Repository: query.Get("repo"), Kind: query.Get("kind"), State: query.Get("state"), Query: query.Get("query"), Limit: 50}
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

func (server *Server) handleGrants(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	filter := GrantFilter{
		Repository: query.Get("repo"),
		SHA:        query.Get("sha"),
		Status:     query.Get("status"),
	}
	value, err := server.views.Grants(request.Context(), actorFrom(request.Context()), filter)
	server.writeView(writer, value, err)
}

func (server *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	value, err := server.views.Status(request.Context(), actorFrom(request.Context()))
	server.writeView(writer, value, err)
}

func (server *Server) writeView(writer http.ResponseWriter, value any, err error) {
	if server.refuseDuringDrain(writer) {
		return
	}
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
	// The shell names the current asset versions, so no cache may keep it.
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_, _ = io.WriteString(writer, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Oberth Dashboard</title><link rel="stylesheet" href="`+versionedAssetURL("app.css")+`"></head><body data-version="`+html.EscapeString(server.version)+`"><div class="app"><header class="top"><button class="brand" data-route="/runs"><span class="mark">[▲]</span><span>oberth</span><span id="brandSub" class="sub">/ dashboard</span></button><nav class="tabs"><button id="tabRuns" class="tab" data-route="/runs">runs</button><button id="tabRepos" class="tab" data-route="/repos">repos</button><button id="tabIssues" class="tab" data-route="/issues">issues</button><button id="tabStatus" class="tab" data-route="/status">status</button></nav><div class="top-right"><div class="top-status" aria-label="System status"><span class="status-segment backend" title="loading" aria-label="loading"><span id="connLed" class="led" aria-hidden="true"></span><span id="connText">api</span></span><span class="status-divider" aria-hidden="true"></span><span id="versionStatus" class="status-segment version-mode unknown" role="status" aria-live="polite"><span>oberth</span> <span id="verLabel" class="ver">...</span></span></div><button class="iconbtn" data-action="toggle-theme" title="Theme">T</button><button class="iconbtn" data-action="refresh" title="Refresh">R</button><button class="iconbtn" data-action="clear-token" title="Sign out">L</button></div></header><main id="app" class="main" data-view="`+view+`"><noscript><p>The Oberth dashboard requires JavaScript. The JSON views remain available at /api/runs, /api/repos, /api/issues, /api/grants, and /api/status with a bearer token.</p></noscript></main></div><dialog id="issueDialog" class="dlg" aria-labelledby="issueDialogTitle"><div class="dlg-shell"><header class="dlg-head"><h2 id="issueDialogTitle">Issue</h2><span id="issueDialogMeta" class="meta"></span><button class="dlg-close" data-action="close-issue" aria-label="Close issue details">×</button></header><div id="issueDialogContent" class="dlg-body"></div></div></dialog><script src="`+versionedAssetURL("app.js")+`" defer></script></body></html>`)
}

// etagMatches is the weak comparison If-None-Match requires (RFC 9110
// 13.1.2): a CDN may hand back W/"…", or a list of tags.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// serveAsset serves the embedded dashboard assets with explicit content types
// so nosniff never blocks them. The request name is only a key into the fixed
// startup-materialized asset map: unknown names are 404s and no request value
// ever reaches a filesystem read. A URL carrying the asset's current version
// is immutable; any other URL (unversioned, such as the fonts app.css names,
// or an old version) must be revalidated, which the ETag keeps cheap (#054).
func serveAsset(writer http.ResponseWriter, request *http.Request) {
	asset, ok := staticAssets[request.PathValue("asset")]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	etag := `"` + asset.version + `"`
	writer.Header().Set("Content-Type", asset.contentType)
	writer.Header().Set("ETag", etag)
	switch {
	case request.URL.Query().Get("v") == asset.version:
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case asset.contentType == "font/woff2":
		// Fonts are linked by fixed names from app.css and never change
		// under a name; keep the previous hour of caching for them.
		writer.Header().Set("Cache-Control", "public, max-age=3600")
	default:
		writer.Header().Set("Cache-Control", "no-cache")
	}
	if etagMatches(request.Header.Get("If-None-Match"), etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = writer.Write(asset.body)
}
