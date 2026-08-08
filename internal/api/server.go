package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxRequestBytes = 1 << 20

type Actor struct {
	Identity    string `json:"identity"`
	Fingerprint string `json:"fingerprint"`
}

type Authenticator interface {
	Authenticate(context.Context, string) (Actor, error)
}

type ToolService interface {
	CallTool(context.Context, Actor, string, json.RawMessage) (any, error)
}

type IssueFilter struct {
	Repository string
	Kind       string
	State      string
	BeforeID   int
	Limit      int
}

type ViewService interface {
	Runs(context.Context, Actor, int) (any, error)
	Repositories(context.Context, Actor) (any, error)
	Issues(context.Context, Actor, IssueFilter) (any, error)
	Status(context.Context, Actor) (any, error)
	Ready(context.Context) error
}

type Server struct {
	auth    Authenticator
	tools   ToolService
	views   ViewService
	version string
	mux     *http.ServeMux
}

type contextKey struct{}

func New(auth Authenticator, tools ToolService, views ViewService, version string) (*Server, error) {
	if auth == nil || tools == nil || views == nil {
		return nil, errors.New("authenticator, tool service, and view service are required")
	}
	if version == "" {
		version = "dev"
	}
	server := &Server{auth: auth, tools: tools, views: views, version: version, mux: http.NewServeMux()}
	server.routes()
	return server, nil
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
	server.mux.Handle("GET /api/repos", server.requireAuth(http.HandlerFunc(server.handleRepos)))
	server.mux.Handle("GET /api/issues", server.requireAuth(http.HandlerFunc(server.handleIssues)))
	server.mux.Handle("GET /api/status", server.requireAuth(http.HandlerFunc(server.handleStatus)))
	server.mux.HandleFunc("GET /assets/oberth.css", serveCSS)
	server.mux.HandleFunc("GET /assets/oberth.js", serveJavaScript)
	server.mux.HandleFunc("GET /{$}", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/runs", http.StatusTemporaryRedirect)
	})
	for _, path := range []string{"/runs", "/repos", "/issues", "/status"} {
		path := path
		server.mux.HandleFunc("GET "+path, func(writer http.ResponseWriter, _ *http.Request) { servePage(writer, strings.TrimPrefix(path, "/")) })
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
	value, err := server.views.Runs(request.Context(), actorFrom(request.Context()), queryLimit(request, 100, 500))
	writeView(writer, value, err)
}

func (server *Server) handleRepos(writer http.ResponseWriter, request *http.Request) {
	value, err := server.views.Repositories(request.Context(), actorFrom(request.Context()))
	writeView(writer, value, err)
}

func (server *Server) handleIssues(writer http.ResponseWriter, request *http.Request) {
	filter, err := issueFilter(request)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid issue filter"})
		return
	}
	value, err := server.views.Issues(request.Context(), actorFrom(request.Context()), filter)
	writeView(writer, value, err)
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
	writeView(writer, value, err)
}

func writeView(writer http.ResponseWriter, value any, err error) {
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request failed"})
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

func servePage(writer http.ResponseWriter, view string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_, _ = io.WriteString(writer, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Oberth</title><link rel="stylesheet" href="/assets/oberth.css"></head><body><nav><strong>Oberth</strong><a href="/runs">Runs</a><a href="/repos">Repositories</a><a href="/issues">Issues</a><a href="/status">Status</a></nav><main data-view="`+view+`"><h1>`+strings.ToUpper(view[:1])+view[1:]+`</h1><form id="token-form"><label>Uplink token <input id="token" type="password" autocomplete="off"></label><button>Connect</button></form><p id="message">Enter an uplink token to load this view.</p><pre id="content" hidden></pre></main><script src="/assets/oberth.js" defer></script></body></html>`)
}

func serveCSS(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/css; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(writer, `:root{color-scheme:dark;font:16px/1.5 system-ui,sans-serif;background:#0b1118;color:#e7edf4}body{max-width:1100px;margin:auto;padding:24px}nav{display:flex;gap:18px;align-items:center}nav strong{font-size:1.3rem;margin-right:auto}a{color:#8fd8b5}main{margin-top:36px}form{display:flex;gap:10px;align-items:end;background:#151e28;padding:16px;border-radius:10px}label{display:grid;gap:6px;flex:1}input,button{font:inherit;padding:9px;border-radius:6px;border:1px solid #405064;background:#0b1118;color:inherit}button{cursor:pointer;background:#174631}pre{white-space:pre-wrap;overflow:auto;background:#111923;padding:16px;border-radius:10px}`)
}

func serveJavaScript(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(writer, `"use strict";const main=document.querySelector("main[data-view]");const form=document.getElementById("token-form");const input=document.getElementById("token");const message=document.getElementById("message");const content=document.getElementById("content");const endpoint="/api/"+main.dataset.view;async function load(token){message.textContent="Loading…";content.hidden=true;try{const response=await fetch(endpoint,{headers:{Authorization:"Bearer "+token},cache:"no-store"});if(!response.ok){throw new Error(response.status===401?"Token rejected":"Request failed ("+response.status+")")}const body=await response.json();content.textContent=JSON.stringify(body,null,2);content.hidden=false;message.textContent=""}catch(error){message.textContent=error.message}}form.addEventListener("submit",event=>{event.preventDefault();const token=input.value;input.value="";load(token)});`)
}
