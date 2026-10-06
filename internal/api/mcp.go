package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

const mcpProtocolVersion = "2025-03-26"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// Meta is protocol-level request metadata, not part of a tool's arguments.
	Meta map[string]json.RawMessage `json:"_meta,omitempty"`
}

// PromoteResponse is intentionally only the durable promotion identifier.
// Promotion state is retrieved through promote_status rather than exposing the
// internal persistence record.
type PromoteResponse struct {
	ID string `json:"id"`
}

// PromotionListResponse recovers durable admission IDs without exposing actor,
// failure text or internal storage fields. Listing never starts a promotion.
type PromotionListItem struct {
	ID           string `json:"id"`
	Sequence     int64  `json:"sequence"`
	RepoID       int64  `json:"repo_id"`
	SourceBranch string `json:"source_branch"`
	SourceSHA    string `json:"source_sha"`
	TargetRef    string `json:"target_ref"`
	ResultSHA    string `json:"result_sha,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}
type PromotionListResponse struct {
	Promotions []PromotionListItem `json:"promotions"`
	NextBefore int64               `json:"next_before,omitempty"`
}

// PublishRetryResponse reports the outcome of retrying a failed upstream
// publication. The publication ID is the new durable record created for the
// retry; status and error describe the terminal outcome.
type PublishRetryResponse struct {
	RunID         string `json:"run_id,omitempty"`
	PromotionID   string `json:"promotion_id,omitempty"`
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

// IssueCreateResponse is the response for issue_create.
type IssueCreateResponse struct {
	ID int64 `json:"id"`
}

// IssueResponse is the complete issue record exposed to MCP clients. Internal
// projection sequence and storage ownership fields stay behind the service.
type IssueResponse struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	RepoID      int64  `json:"repo_id,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Occurrences int    `json:"occurrences,omitempty"`
	CIOrigin    string `json:"ci_origin,omitempty"`
	CIWorkID    string `json:"ci_work_id,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// IssueGetManyMaximumBytes bounds encoded structured content, including all
// result rows. Complete records that do not fit use an explicit fallback row.
const IssueGetManyMaximumBytes = 256 << 10

type IssueGetManyResult struct {
	ID    int64          `json:"id"`
	Issue *IssueResponse `json:"issue,omitempty"`
	Error string         `json:"error,omitempty"`
}

type IssueGetManyResponse struct {
	Results []IssueGetManyResult `json:"results"`
}

type IssueListItem struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Kind        string `json:"kind,omitempty"`
	Title       string `json:"title,omitempty"`
	RepoID      int64  `json:"repo_id,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Occurrences int    `json:"occurrences,omitempty"`
	CIOrigin    string `json:"ci_origin,omitempty"`
	CIWorkID    string `json:"ci_work_id,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type IssueListResponse struct {
	Issues     []IssueListItem `json:"issues"`
	NextBefore int64           `json:"next_before,omitempty"`
}

type IssueLockResponse struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner"`
	ExpiresAt string `json:"expires_at"`
	Renewed   bool   `json:"renewed"`
}

// AccessGrantResponse is the wire representation of a secret access grant.
type AccessGrantResponse struct {
	ID         int64   `json:"id"`
	Repo       string  `json:"repo"`
	Step       string  `json:"step"`
	Secret     string  `json:"secret"`
	ApprovedBy string  `json:"approved_by"`
	ApprovedAt string  `json:"approved_at"`
	RevokedBy  string  `json:"revoked_by,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	Warning    string  `json:"warning,omitempty"`
}

// AccessListResponse wraps a list of grants.
type AccessListResponse struct {
	Grants []AccessGrantResponse `json:"grants"`
}

// RunListResponse wraps run_list results as a JSON object so MCP
// structuredContent satisfies the record requirement (#794).
type RunListResponse struct {
	Runs any `json:"runs"`
}

// RepoListResponse wraps repo_list results as a JSON object (#794).
type RepoListResponse struct {
	Repositories any `json:"repositories"`
}

func (server *Server) handleMCP(writer http.ResponseWriter, request *http.Request) {
	// MaxBytesReader enforces the body limit at the HTTP layer, returning a
	// 413 and closing the connection on oversize bodies rather than allowing a
	// trickle-body to hold a goroutine indefinitely. The server's ReadTimeout
	// bounds the time dimension; this bounds the byte dimension.
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	var incoming rpcRequest
	if err := decodeOne(request.Body, &incoming); err != nil {
		writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	if incoming.JSONRPC != "2.0" || incoming.Method == "" {
		writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: normalizeID(incoming.ID), Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	if len(incoming.ID) == 0 {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	response := rpcResponse{JSONRPC: "2.0", ID: incoming.ID}
	switch incoming.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "oberth", "version": server.version},
			"instructions":    "Use status/wait/logs for SHA-oriented CI, run_get/run_logs for exact promotion run IDs, sync only to park the selected exact WIP branch upstream without a green gate, and promote to merge, test, and push a target branch. Sync is not completion evidence. Filter logs with pattern/context/offset/limit/tail rather than retrieving a whole step; a step can exceed a context window and the response reports what it withheld. A step named frag-<hash>-<template> came from another repository's pinned pipeline fragment, not from the repository under test, so a failure there is usually the pinned version rather than this commit. Authenticated JSON dashboard state is available at /api/runs, /api/repos, /api/issues, and /api/status.",
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": toolDefinitions()}
	case "tools/call":
		var call callParams
		if err := strictUnmarshal(incoming.Params, &call); err != nil || call.Name == "" {
			response.Error = &rpcError{Code: -32602, Message: "invalid tools/call params"}
			break
		}
		if !knownTool(call.Name) {
			response.Result = toolFailure(errors.New("unknown tool " + call.Name))
			break
		}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage("{}")
		}
		// Extend the write deadline for long-poll tools so the HTTP
		// server's default WriteTimeout does not terminate the response
		// before the requested wait duration expires (#789). The margin
		// is derived from the service wait ceiling so the two cannot
		// drift independently (#793).
		if call.Name == "wait" || call.Name == "promote_status" {
			rc := http.NewResponseController(writer)
			if err := rc.SetWriteDeadline(time.Now().Add(server.maximumToolWait + writeDeadlineMargin)); err != nil {
				log.Printf("mcp: SetWriteDeadline: %v (long-poll may be truncated by WriteTimeout)", err)
			}
		}
		result, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), call.Name, call.Arguments)
		if err != nil {
			response.Result = server.classifyToolFailure(err)
		} else {
			response.Result = toolSuccess(result)
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	writeRPC(writer, response)
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeRPC(writer http.ResponseWriter, response rpcResponse) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}

func toolSuccess(payload any) map[string]any {
	isError := false
	if value, ok := payload.(interface{ MCPToolIsError() bool }); ok {
		isError = value.MCPToolIsError()
	}
	if value, ok := payload.(interface{ MCPToolText() string }); ok {
		return map[string]any{
			"content":           []map[string]string{{"type": "text", "text": value.MCPToolText()}},
			"structuredContent": payload,
			"isError":           isError,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return toolFailure(err)
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}, "structuredContent": payload, "isError": false}
}

// classifyToolFailure applies the same error classifier the dashboard API uses,
// so MCP tool errors never leak raw internal error chains (sqlite context, file
// paths, internal state). Actionable classes (invalid input, not found, lock-owned,
// ambiguous) keep their message; unknown errors are logged server-side with a
// correlation ID and returned as a generic message carrying that ID.
func (server *Server) classifyToolFailure(err error) map[string]any {
	code, message := server.classifyError(err)
	if code == http.StatusInternalServerError {
		correlationID := requestCorrelationID()
		log.Printf("mcp: tool error [%s]: %v", correlationID, err)
		message = fmt.Sprintf("internal error (reference %s)", correlationID)
	}
	return toolFailure(errors.New(message))
}

func toolFailure(err error) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
}

func requestCorrelationID() string {
	var body [8]byte
	_, _ = rand.Read(body[:])
	return hex.EncodeToString(body[:])
}

func knownTool(name string) bool {
	for _, definition := range toolDefinitions() {
		if definition["name"] == name {
			return true
		}
	}
	return false
}

func toolDefinitions() []map[string]any {
	stringProperty := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	integerProperty := func(description string) map[string]any {
		return map[string]any{"type": "integer", "description": description}
	}
	logFilterProperties := func(properties map[string]any) map[string]any {
		properties["pattern"] = stringProperty("RE2 pattern; only matching lines are returned, matched against the line with its [burn/step] prefix removed")
		properties["context"] = integerProperty("Lines of context each side of a match")
		properties["offset"] = integerProperty("First line to return, 0-based; pages through matches when pattern is set")
		properties["limit"] = integerProperty("Maximum lines to return")
		properties["tail"] = map[string]any{"type": "boolean", "description": "Take from the end of the step instead of the start"}
		return properties
	}
	timeoutProperty := func() map[string]any {
		return map[string]any{"type": "integer", "description": "Timeout seconds (maximum 600)", "minimum": 1, "maximum": 600}
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema}
	}
	return []map[string]any{
		tool("status", "Return concise CI status for a full SHA, short SHA, or branch, including the failed step.", object(map[string]any{"repo": stringProperty("Repository name when the selector is ambiguous"), "ref": stringProperty("Full SHA, short SHA, or branch")}, "ref")),
		tool("logs", "Return one named step log for a SHA, optionally filtered server-side. Prefer a pattern over retrieving a whole step: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one.", object(logFilterProperties(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "step": stringProperty("Step name")}), "sha", "step")),
		tool("run_get", "Return one run and its named step results by full durable run ID or unique hexadecimal prefix (at least 12 characters).", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 12 characters)")}, "id")),
		tool("artifacts", "List the files a run kept: test reports, coverage output, anything the pipeline copied to OBERTH_ARTIFACTS. Cheap; call this before artifact_get.", object(map[string]any{"id": stringProperty("Exact run ID")}, "id")),
		tool("artifact_get", "Return one artifact's contents by run ID and name, optionally filtered server-side. Prefer a pattern over retrieving a whole file: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one.", object(logFilterProperties(map[string]any{"id": stringProperty("Exact run ID"), "name": stringProperty("Artifact name as reported by artifacts")}), "id", "name")),
		tool("run_logs", "Return one exact burn/step log by full run ID or unique hexadecimal prefix (at least 12 characters), optionally filtered server-side. Active steps without retained output return an actionable pending error; retry after the step completes. Prefer a pattern over retrieving a whole step: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one.", object(logFilterProperties(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 12 characters)"), "burn": stringProperty("Burn name"), "step": stringProperty("Step name")}), "id", "burn", "step")),
		tool("wait", "Long-poll until a SHA reaches a terminal state; timeout returns still-running cleanly. When trigger is set, waits for a run with that trigger (e.g. 'release' for a tag-push run).", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "trigger": stringProperty("Filter by trigger type (e.g. 'release' for tag runs, 'branch' for branch runs; 'ci' is accepted as an alias for 'branch')"), "timeout": timeoutProperty()}, "sha")),
		tool("sync", "Park the exact SHA's WIP branch upstream without a green gate; this is not completion or promotion evidence. Rejects promotion, release, plan, and apply runs. When the SHA has branch-trigger runs on multiple distinct branches, the explicit branch argument is required.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Explicit branch name; required when the SHA has runs on multiple branches, optional otherwise")}, "sha")),
		tool("promote", "Green-gate and publish a SHA without force.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Target branch")}, "sha", "branch")),
		tool("promotion_list", "Recover durable promotion IDs by exact repository/source SHA/status. Read-only, newest admission first; pass next_before as before for older records. A record is not publication evidence.", object(map[string]any{
			"repo":       stringProperty("Filter by exact repository name"),
			"source_sha": stringProperty("Filter by full source commit SHA; no ref or prefix resolution"),
			"status":     map[string]any{"type": "string", "enum": []string{"pending", "passed", "failed", "interrupted"}, "description": "Filter by current promotion status"},
			"before":     map[string]any{"type": "integer", "minimum": 1, "description": "Exclusive admission sequence cursor returned as next_before"},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50, maximum 200)"},
		})),
		tool("promote_status", "Wait for an append-only promotion record to become terminal.", object(map[string]any{"id": stringProperty("Promotion ID"), "timeout": timeoutProperty()}, "id")),
		tool("issue_create", "Create a global manual issue.", object(map[string]any{"title": stringProperty("Issue title"), "body": stringProperty("Issue body")}, "title", "body")),
		tool("issue_get", "Get an issue by ID.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_get_many", "Get complete issue records for 1..50 unique positive IDs in request order. Each result contains issue or error (not_found or response_limit). Structured JSON is bounded to 256 KiB; use issue_get for response_limit IDs. Bodies are never truncated. Read-only; does not renew locks.", object(map[string]any{"ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1}}}, "ids")),
		tool("issue_update", "Update an issue title and body.", object(map[string]any{"id": integerProperty("Issue ID"), "title": stringProperty("Issue title"), "body": stringProperty("Issue body")}, "id", "title", "body")),
		tool("issue_close", "Close an issue without deleting its history.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_reopen", "Reopen a manual issue without changing its ID or history. Already-open manual issues are unchanged; CI issues cannot be reopened manually.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_delete", "Delete an accidentally created manual issue.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_list", "List issue metadata, newest creation ID first (not latest updated). Filters apply before pagination. Defaults to all issues, 50 per page; pass next_before as before with the same filters until absent for a complete queue scan.", object(map[string]any{
			"repo":   stringProperty("Exact repository name; global manual issues have no repository"),
			"kind":   map[string]any{"type": "string", "enum": []string{"manual", "ci"}},
			"state":  map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}},
			"before": map[string]any{"type": "integer", "minimum": 0, "description": "Exclusive issue ID cursor; zero starts the first page"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Page size (default 50)"},
		})),
		tool("issue_lock", "Acquire or renew the caller-owned five-minute issue lock.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("access_list", "List secret access grants for a repository.", object(map[string]any{"repo": stringProperty("Repository name (empty lists all)"), "revoked": map[string]any{"type": "boolean", "description": "Include revoked grants"}})),
		tool("access_allow", "Grant a step access to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "step": stringProperty("Step/template name"), "secret": stringProperty("Short secret path (e.g. terraform/credentials)")}, "repo", "step", "secret")),
		tool("access_revoke", "Revoke a step's access to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "step": stringProperty("Step/template name"), "secret": stringProperty("Short secret path (e.g. terraform/credentials)")}, "repo", "step", "secret")),
		tool("repo_list", "List registered repositories with their upstream and probe state.", object(map[string]any{})),
		tool("repo_remove", "Remove a repository mapping and its Git cache. Requires admin uplink. Refuses if runs are in-flight or promotions are pending.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("run_list", "List recent runs with optional repository and ref filters, bounded page.", object(map[string]any{
			"repo":  stringProperty("Filter by repository name"),
			"ref":   stringProperty("Filter by branch or tag ref"),
			"limit": integerProperty("Maximum runs to return (default 50, max 200)"),
		})),
		tool("system_status", "Return system health: database, upstreams, cluster, audit, and version.", object(map[string]any{})),
		tool("publish_retry", "Retry the upstream publication for a run or promotion whose burns all passed but whose upstream push failed. Input is exactly one durable run ID or promotion ID. Refuses when the run is not in a publishing-failed state, or when the cached Git object no longer matches the record.", object(map[string]any{
			"id": stringProperty("Durable run ID or promotion ID whose publication failed"),
		}, "id")),
		tool("secretstore_plan", "Compute the policy/role diff that `oberth secretstore sync` would apply based on current approved grants. Admin uplink required. Read-only: the server never receives an admin Bao token. Returns per-repo policy names and paths; never secret values.", object(map[string]any{})),
		tool("secretstore_sync_receipt", "Record a secretstore.sync materialization receipt in the audit chain. Admin uplink required. The server recomputes the plan digest and records status current or stale. Posted by `oberth secretstore sync` after a successful sync.", object(map[string]any{
			"plan_digest":   stringProperty("SHA-256 plan digest from the sync"),
			"policies":      map[string]any{"type": "object", "description": "Map of policy/role name to changed boolean", "additionalProperties": map[string]any{"type": "boolean"}},
			"changed_count": integerProperty("Number of policies/roles written"),
			"total_count":   integerProperty("Total policies/roles evaluated"),
		}, "plan_digest")),
		tool("secretstore_verify", "Verify real secret-store login and KV reads using configured server or release-tier trust. Admin uplink required. Returns paths/key counts, optionally field names; never secret values. Does not change infrastructure or grants.", object(map[string]any{
			"paths":        map[string]any{"type": "array", "items": stringProperty("KV API path or virtual oberth/upstream/... path"), "maxItems": 32, "description": "Paths to verify; defaults to the server's configured verification paths"},
			"release_tier": map[string]any{"type": "boolean", "description": "Use the configured release-tier trust and ServiceAccount; pair with repo for real per-repo preflight"},
			"repo":         stringProperty("Exact upstream/org/repo identity; requires release_tier"),
			"tier":         map[string]any{"type": "string", "enum": []string{"release", "ci"}, "default": "release", "description": "Per-repo identity tier; ci requires release_tier and repo"},
			"keys":         map[string]any{"type": "boolean", "description": "List field names (server tier only)"},
			"expect":       map[string]any{"type": "array", "items": stringProperty("Expected <path-base>/<field>[,<field>,...]"), "maxItems": 32, "description": "Assert field names; implies keys (server tier only)"},
			"timeout":      map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "default": 45, "description": "Overall deadline in seconds, including TokenRequest and login"},
		})),
	}
}
