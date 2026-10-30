package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/oberthci/oberth/internal/model"
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

// PromoteRetryResponse identifies the new promotion that re-runs a failed or
// interrupted one (#1097). Follow it with promote_status.
type PromoteRetryResponse struct {
	ID      string `json:"id"`
	RetryOf string `json:"retry_of"`
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
	// NextCursor replaces NextBefore for a scoped uplink: the oldest
	// promotion's ID, never a global sequence (#073).
	NextCursor string `json:"next_cursor,omitempty"`
}

// PublishRetryResponse reports the outcome of retrying a failed upstream
// publication. The publication ID is the new durable record created for the
// retry. A pending status means delivery/finalization is still being recovered;
// poll run_get or wait using the returned run ID for its eventual outcome.
type PublishRetryResponse struct {
	RunID         string `json:"run_id,omitempty"`
	PromotionID   string `json:"promotion_id,omitempty"`
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

// RunCancelResponse is the response for run_cancel. Status stays the durable
// terminal status (interrupted); Phase "cancelled" marks an explicit cancel.
// Workflow is none (never claimed), not_submitted (claimed, no Workflow was
// created), deleted (deletion accepted) or pending (the scheduler's
// cancellation pass retries the deletion).
type RunCancelResponse struct {
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	Phase         string `json:"phase"`
	PreviousState string `json:"previous_state"`
	CancelledBy   string `json:"cancelled_by"`
	Reason        string `json:"reason"`
	Workflow      string `json:"workflow"`
	PromotionID   string `json:"promotion_id,omitempty"`
	Warning       string `json:"warning,omitempty"`
}

// RunRerunResponse is the response for run_rerun (#1098). Mode "rerun"
// queued RunID at the same SHA and ref as RerunOf; mode "publish_retry" means
// every step of RerunOf had passed and only its upstream publication failed,
// so that publication was retried (PublicationID, Status, Error) without
// re-running anything.
type RunRerunResponse struct {
	Mode          string `json:"mode"`
	RunID         string `json:"run_id,omitempty"`
	RerunOf       string `json:"rerun_of"`
	Status        string `json:"status"`
	Ref           string `json:"ref,omitempty"`
	SHA           string `json:"sha,omitempty"`
	PublicationID string `json:"publication_id,omitempty"`
	Warning       string `json:"warning,omitempty"`
	Error         string `json:"error,omitempty"`
}

// IssueCreateResponse is the response for issue_create.
type IssueCreateResponse struct {
	ID int64 `json:"id"`
}

// IssueResponse is the complete issue record exposed to MCP clients. Internal
// projection sequence and storage ownership fields stay behind the service.
type IssueResponse struct {
	Lock        *model.IssueLock `json:"lock"`
	ID          int64            `json:"id"`
	Upstream    string           `json:"upstream"`
	Kind        string           `json:"kind"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	State       string           `json:"state"`
	RepoID      int64            `json:"repo_id,omitempty"`
	Branch      string           `json:"branch,omitempty"`
	Occurrences int              `json:"occurrences,omitempty"`
	CIOrigin    string           `json:"ci_origin,omitempty"`
	CIWorkID    string           `json:"ci_work_id,omitempty"`
	CreatedAt   string           `json:"created_at,omitempty"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
}

// IssueMutationResponse acknowledges a write without echoing issue content.
// AppendedBytes counts caller text bytes (excluding the dated separator), and
// is zero for update/close.
type IssueMutationResponse struct {
	ID            int64  `json:"id"`
	State         string `json:"state"`
	UpdatedAt     string `json:"updated_at"`
	Occurrences   int64  `json:"occurrences"`
	AppendedBytes int    `json:"appended_bytes"`
}

// IssueGetManyMaximumBytes bounds encoded structured content, including all
// result rows. Records that do not fit use an explicit fallback row.
const IssueGetManyMaximumBytes = 256 << 10

type IssueGetManyResult struct {
	ID    int64  `json:"id"`
	Issue any    `json:"issue,omitempty"` // IssueResponse or a fields projection.
	Error string `json:"error,omitempty"`
}

type IssueGetManyResponse struct {
	Results []IssueGetManyResult `json:"results"`
}

type IssueListItem struct {
	Lock        *model.IssueLock `json:"lock"`
	ID          int64            `json:"id"`
	Upstream    string           `json:"upstream"`
	State       string           `json:"state"`
	Kind        string           `json:"kind,omitempty"`
	Title       string           `json:"title,omitempty"`
	RepoID      int64            `json:"repo_id,omitempty"`
	Branch      string           `json:"branch,omitempty"`
	Occurrences int              `json:"occurrences,omitempty"`
	CIOrigin    string           `json:"ci_origin,omitempty"`
	CIWorkID    string           `json:"ci_work_id,omitempty"`
	CreatedAt   string           `json:"created_at,omitempty"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
}

type IssueListResponse struct {
	Issues     []IssueListItem   `json:"issues"`
	NextBefore int64             `json:"next_before,omitempty"`
	Total      int64             `json:"total"`
	Counts     model.IssueCounts `json:"counts"`
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

// AccessListPendingResponse wraps pending (declared-but-not-granted) entries.
// Issue #623, finding 4.
type AccessListPendingResponse struct {
	Declarations []PendingDeclaration `json:"declarations"`
}

// PendingDeclaration describes one declared-but-not-granted (repo, step, path).
type PendingDeclaration struct {
	Repo        string `json:"repo"`
	Step        string `json:"step"`
	Path        string `json:"path"`
	LastSeenSHA string `json:"last_seen_sha"`
	LastSeenAt  string `json:"last_seen_at"`
}

// PromotionGrantListResponse keeps even an empty grant list a JSON object.
type PromotionGrantListResponse struct {
	Grants []map[string]any `json:"grants"`
}

// IssueDeleteResponse acknowledges a successful deletion without a null result.
type IssueDeleteResponse struct {
	ID      int64 `json:"id"`
	Deleted bool  `json:"deleted"`
}

// RunListResponse wraps run_list results as a JSON object so MCP
// structuredContent satisfies the record requirement (#794).
type RunListResponse struct {
	Runs any `json:"runs"`
	// NextBefore is set when the page is full: pass it as before for the
	// next, older page (#050).
	NextBefore int64 `json:"next_before,omitempty"`
	// NextCursor replaces NextBefore for a scoped uplink: the oldest run's
	// ID, never a global sequence (#073).
	NextCursor string `json:"next_cursor,omitempty"`
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
			"instructions":    "Use status/wait/logs for SHA-oriented CI, run_get/run_logs for exact promotion run IDs, sync only to park the selected exact WIP branch upstream without a green gate, and promote to merge, test, and push a target branch. Sync is not completion evidence. Prefer issue_get/issue_get_many with fields/body_limit and run_get with summary:true for routine status reads; full records remain available. Filter logs with pattern/context/limit/tail rather than retrieving a whole step. Log reads scan bounded byte windows; counts are lower bounds unless scan_complete is true. When has_more is true, continue with next_byte_offset as byte_offset, offset=0 and the same tail/pattern/context, including after an empty window. offset skips only within the current scanned window; with tail that window can change during lookback. An empty incomplete page does not prove absence. A step named frag-<hash>-<template> came from another repository's pinned pipeline fragment, not from the repository under test, so a failure there is usually the pinned version rather than this commit. Authenticated JSON dashboard state is available at /api/runs, /api/repos, /api/issues, /api/grants, and /api/status.",
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
		if server.refuseDuringDrain(writer) {
			return
		}
		if call.Name == "wait" || call.Name == "promote_status" {
			server.handleMCPLongPoll(writer, request, response, call)
			return
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
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return toolFailure(errors.New("internal error: tool result must be a JSON object"))
	}
	isError := false
	if value, ok := payload.(interface{ MCPToolIsError() bool }); ok {
		isError = value.MCPToolIsError()
	}
	if value, ok := payload.(interface{ MCPToolText() string }); ok {
		return map[string]any{
			"content":           []map[string]string{{"type": "text", "text": value.MCPToolText()}},
			"structuredContent": json.RawMessage(raw),
			"isError":           isError,
		}
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}, "structuredContent": json.RawMessage(raw), "isError": false}
}

// classifyToolFailure applies the same error classifier the dashboard API uses,
// so MCP tool errors never leak raw internal error chains (sqlite context, file
// paths, internal state). Actionable classes (invalid input, not found, lock-owned,
// ambiguous) keep their message; unknown errors are logged server-side with a
// correlation ID and returned as a generic message carrying that ID. Only
// explicitly public service reasons extend validation/precondition classes.
func (server *Server) classifyToolFailure(err error) map[string]any {
	code, message := server.classifyError(err)
	message = toolRefusalMessage(err, code, message)
	if code == http.StatusInternalServerError {
		correlationID := requestCorrelationID()
		log.Printf("mcp: tool error [%s]: %v", correlationID, err)
		message = fmt.Sprintf("internal error (reference %s)", correlationID)
	}
	return toolFailure(errors.New(message))
}

// toolRefusalMessage is shared by ordinary calls and both long-poll transports.
// Only explicitly public service reasons may extend a refusal. Never use the
// raw error string: it can contain dependencies or inaccessible resources.
func toolRefusalMessage(err error, code int, message string) string {
	var reason interface{ MCPErrorReason() string }
	if !errors.As(err, &reason) {
		return message
	}
	switch code {
	case http.StatusBadRequest:
		// These classifiers may return err.Error(). Replace it rather than
		// appending to it: the marked refusal can wrap a private cause.
		return "invalid input: " + reason.MCPErrorReason()
	case http.StatusConflict:
		return "conflict: " + reason.MCPErrorReason()
	case http.StatusForbidden, http.StatusServiceUnavailable:
		return message + ": " + reason.MCPErrorReason()
	}
	return message
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

// ToolNames lists every MCP tool the server offers, so the service can prove
// each one is classified for scoped uplinks (#073).
func ToolNames() []string {
	definitions := toolDefinitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition["name"].(string))
	}
	return names
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
		properties["offset"] = integerProperty("Skip lines or matches only within the current scanned window, not globally from the step start; tail lookback can change that window; use offset:0 with byte_offset continuations")
		if _, artifact := properties["name"]; !artifact {
			properties["byte_offset"] = integerProperty("Absolute retained-run byte cursor from next_byte_offset; head scans move forward, tail scans move backward; reset offset to zero when continuing")
		}
		properties["limit"] = integerProperty("Maximum lines to return")
		properties["tail"] = map[string]any{"type": "boolean", "description": "Take from the end of the step instead of the start"}
		return properties
	}
	timeoutProperty := func() map[string]any {
		return map[string]any{"type": "integer", "description": "Timeout seconds; omitted or zero defaults to 600; values above 600 are rejected", "minimum": 0, "maximum": 600, "default": 600}
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema, "outputSchema": toolOutputSchema(name)}
	}
	return []map[string]any{
		tool("status", "Return concise CI status for a full SHA, short SHA, or branch, including the failed step.", object(map[string]any{"repo": stringProperty("Repository name when the selector is ambiguous"), "ref": stringProperty("Full SHA, short SHA, or branch")}, "ref")),
		tool("logs", "Return one named step log for a SHA, optionally filtered server-side. Prefer a pattern over retrieving a whole step: counts are lower bounds unless scan_complete is true; continue has_more pages with next_byte_offset as byte_offset and offset:0.", object(logFilterProperties(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "step": stringProperty("Step name")}), "sha", "step")),
		tool("run_kvm_preflight", "Read bounded immutable KVM preflight evidence for one full run ID or unique hexadecimal prefix (at least 8 characters). Receipt validity and cleanup are historical evidence, not publication eligibility; parent status is separate. A known run without a preflight returns present=false.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)")}, "id")),
		tool("run_get", "Return one run by full durable ID or unique hexadecimal prefix (at least 8 characters). Default: full run, step results and versioned retained timing; missing timing is unknown. Set summary:true for only id/status/phase/failed_burn/failed_step/error and steps (burn, name, status, started_at, finished_at), omitting Timing, repository and diagnostics.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "summary": map[string]any{"type": "boolean", "description": "Return only run status/failure and compact steps; default false"}}, "id")),
		tool("run_retry", "Queue a fresh attempt for the newest interrupted or infrastructure-failed branch run, retaining its repository, ref and SHA. Workflow-level failures without a failed step qualify; test failures, older runs, release runs, and branches with an active run are refused. The caller is recorded as actor.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)")}, "id")),
		tool("run_cancel", "Cancel one queued, waiting or running run. The run becomes interrupted with phase cancelled and a reason naming the caller; a claimed run's Workflow is deleted and its admission released, and it is never re-fired. Cancelling a terminal run is refused. Non-admin uplinks may cancel only branch-push runs outside a concurrency group; admins may cancel any run but must set force for tag/release and apply runs, whose publication may be left partial. Cancelling promotion CI fails the promotion without merging. Audited.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"reason": stringProperty("Why the run is cancelled (printable text, at most 512 bytes)"),
			"force":  map[string]any{"type": "boolean", "description": "Admin only: confirm cancelling a tag/release or apply run whose publication may be partial"},
		}, "id")),
		tool("run_rerun", "Rerun a failed or interrupted run at the same SHA and ref, linked to the original (rerun_of), for what run_retry refuses: a failed step such as a flaky test, a run that is no longer the newest of its ref, and tag/release runs. SHA-keyed status, wait and promote then follow the newest run for that SHA. Refused while the ref has an active run, when a branch has moved past the run's SHA, and for promotion (use promote_retry), plan and apply runs. A run that passed every step and failed only in upstream publication is not re-executed: its publication is retried as publish_retry does (mode publish_retry). Non-admin uplinks may rerun branch-push runs; tag/release runs need an admin and force, and a tag that is already published or green is refused, so it is never published twice. Audited as run.rerun.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"reason": stringProperty("Why the run is rerun (printable text, at most 512 bytes)"),
			"force":  map[string]any{"type": "boolean", "description": "Admin only: confirm rerunning a tag/release run, whose Workflow re-executes release.yaml"},
		}, "id")),
		tool("run_purge", "Purge retained logs, artifacts, or both for one terminal run. Requires an unscoped admin uplink and a reason. Active runs, release evidence, promotion evidence, open-issue evidence and active grants are protected. Run and step history stays visible; every purge is audited.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"what":   map[string]any{"type": "string", "enum": []string{"logs", "artifacts", "both"}},
			"reason": stringProperty("Why evidence is purged; required printable text, at most 512 bytes"),
		}, "id", "what", "reason")),
		tool("artifacts", "List the files a run kept: test reports, coverage output, anything the pipeline copied to OBERTH_ARTIFACTS. Cheap; call this before artifact_get.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)")}, "id")),
		tool("artifact_get", "Return one artifact's contents by run ID and name, optionally filtered server-side. Prefer a pattern over retrieving a whole file: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one.", object(logFilterProperties(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "name": stringProperty("Artifact name as reported by artifacts")}), "id", "name")),
		tool("run_logs", "Return one exact burn/step log by full run ID or unique hexadecimal prefix (at least 8 characters), optionally filtered server-side. Active steps without retained output return an actionable pending error. Log discovery/filtering scan at most 4 MiB each per call and stop head pages early on limit. total_scanned_bytes includes both phases; scan_limit_bytes is the aggregate cap (4 MiB terminal, 8 MiB active). Pattern scans allow one concurrent call per bearer token and two globally; waiting shares the 10-second read budget. When has_more is true, continue with next_byte_offset as byte_offset, retaining tail/pattern/context and resetting offset to zero. Counts are lower bounds unless scan_complete is true; bytes is exact only when bytes_exact is true. withheld_bytes/withheld_lines report omitted data. Lines above 64 KiB are omitted and counted.", object(logFilterProperties(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "burn": stringProperty("Burn name (defaults to the failed burn)"), "step": stringProperty("Step name (defaults to the failed step when burn is omitted, or the burn's only step)")}), "id")),
		tool("wait", "Long-poll until a SHA or exact run reaches a terminal state; timeout returns still-running cleanly. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline. Pass id alone to wait for one run by full ID or unique prefix of at least 8 hex characters. When trigger is set, waits for a run with that trigger (e.g. 'release' for a tag-push run).", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters); cannot be combined with repo, sha, or trigger"), "repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA (required unless id is provided)"), "trigger": stringProperty("Filter by trigger type (e.g. 'release' for tag runs, 'branch' for branch runs; 'ci' is accepted as an alias for 'branch')"), "timeout": timeoutProperty()})),
		tool("sync", "Park the exact SHA's WIP branch upstream without a green gate; this is not completion or promotion evidence. Rejects promotion, release, plan, and apply runs. When the SHA has branch-trigger runs on multiple distinct branches, the explicit branch argument is required.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Explicit branch name; required when the SHA has runs on multiple branches, optional otherwise")}, "sha")),
		tool("promote", "Green-gate and publish a SHA without force.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Target branch")}, "sha", "branch")),
		tool("promotion_list", "Recover durable promotion IDs by exact repository/source SHA/status. Read-only, newest admission first; pass next_before (or next_cursor for a scoped uplink) as before for older records. A record is not publication evidence.", object(map[string]any{
			"repo":       stringProperty("Filter by exact repository name"),
			"source_sha": stringProperty("Filter by full source commit SHA; no ref or prefix resolution"),
			"status":     map[string]any{"type": "string", "enum": []string{"pending", "passed", "failed", "interrupted"}, "description": "Filter by current promotion status"},
			"before":     map[string]any{"type": []string{"integer", "string"}, "description": "Exclusive cursor: next_before (an admission sequence) or next_cursor (a promotion ID) from the previous page"},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50, maximum 200)"},
		})),
		tool("promote_status", "Wait for an append-only promotion record to become terminal. A failed or interrupted promotion that was retried names its successor in retried_as. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline.", object(map[string]any{"id": stringProperty("Promotion ID"), "timeout": timeoutProperty()}, "id")),
		tool("promote_retry", "Retry a failed or interrupted promotion: a new promotion of the same repository, source SHA and target, merged onto the target's current head and re-tested like promote, so a moved target or a transient push failure is recovered without a new commit. Same authorization as promote; a grant is consumed only by a delivered promotion, so the original grant authorizes the retry. Refused while a promotion of the same tuple is pending or delivered, or when the source SHA's newest run is not green. Audited as promotion.retry.", object(map[string]any{"id": stringProperty("Promotion ID of the failed or interrupted promotion")}, "id")),
		tool("issue_create", "Create a manual issue, workspace-global or filed under one upstream. Uplinks scoped to that upstream see it; an uplink scoped to one upstream files there by default.", object(map[string]any{"title": stringProperty("Issue title"), "body": stringProperty("Issue body"), "upstream": stringProperty("Upstream name to file the issue under (required for an uplink scoped to several upstreams)")}, "title", "body")),
		tool("issue_get", "Get an issue by ID. Full record by default for compatibility. Optional fields selects only named fields; body_limit caps retained body/occurrence-history bytes with an explicit truncated N bytes marker. The numeric occurrences count remains exact. Read-only; does not renew locks.", object(issueReadProperties("id", integerProperty("Issue ID")), "id")),
		tool("issue_get_many", "Get 1..50 issues in request order, each with issue or error (not_found or response_limit). Full records by default. Optional fields and body_limit apply to each issue before the 256 KiB structured JSON budget; body_limit truncates body/occurrence history with a truncated N bytes marker, keeping numeric occurrences exact. Use compact options or issue_get for response_limit IDs. Read-only; does not renew locks.", object(issueReadProperties("ids", map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1}}), "ids")),
		tool("issue_update", "Update an issue title and body, or move a manual issue to an upstream (empty or global means workspace-global, admin only). A non-admin can move only a visible issue to an upstream in scope. When expected_body_sha256 is set, the server rejects the update if the SHA-256 of the existing body does not match. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID"), "title": stringProperty("Issue title (send with body)"), "body": stringProperty("Issue body (send with title)"), "upstream": stringProperty("New upstream name, or empty/global for workspace-global (admin only)"), "expected_body_sha256": stringProperty("Hex-encoded SHA-256 of the existing body; the server rejects the update on mismatch")}, "id")),
		tool("issue_append", "Append text to an issue body with a dated separator, without replacing the existing content. Safer than issue_update for checkpoints on large issues: the caller never re-sends the existing body. Returns only id, state, updated_at, occurrences, appended_bytes (UTF-8 bytes of text, excluding separator); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID"), "text": stringProperty("Text to append")}, "id", "text")),
		tool("issue_close", "Close an issue without deleting its history. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_reopen", "Reopen a manual issue without changing its ID or history. Already-open manual issues are unchanged; CI issues cannot be reopened manually.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_delete", "Delete an accidentally created manual issue.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_list", "List issue metadata, newest creation ID first (not latest updated), with total and faceted state/kind counts. Filters apply before pagination; counts exclude the cursor. Defaults to all issues, 50 per page; pass next_before as before with the same filters until absent for a complete queue scan.", object(map[string]any{
			"repo":   stringProperty("Exact repository name; global manual issues have no repository"),
			"kind":   map[string]any{"type": "string", "enum": []string{"manual", "ci"}},
			"state":  map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}},
			"query":  stringProperty("Case-insensitive substring in issue title, body or branch"),
			"before": map[string]any{"type": "integer", "minimum": 0, "description": "Exclusive issue ID cursor; zero starts the first page"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Page size (default 50)"},
		})),
		// Issue lifecycle administration (#1103).
		tool("issue_unlock", "Break an issue lock, recording its previous owner. Admin only; visibility and audit gate enforced.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_close_many", "Close 1..50 unique issues with a reason and per-ID results. Admin only; active locks remain enforced, one audit action per closure.", object(map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}, "minItems": 1, "maxItems": 50, "uniqueItems": true}, "reason": stringProperty("Nonempty closure reason, at most 4096 bytes")}, "ids", "reason")),
		tool("issue_lock", "Acquire or renew the caller-owned five-minute issue lock.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		// Credential and grant lifecycle (#1105).
		tool("uplink_list", "List public uplink identities and usage. Unscoped admin only. No tokens are returned.", object(map[string]any{})),
		tool("uplink_revoke", "Revoke an uplink SSH key and bearer immediately. Unscoped admin only; audited; the last active unscoped admin cannot be revoked.", object(map[string]any{"identity": stringProperty("Uplink identity"), "fingerprint": stringProperty("Exact key fingerprint, required when identity has several keys")}, "identity")),
		tool("uplink_rotate", "Finish rotation to an already registered active replacement key with identical identity, admin status and scope, atomically revoking the selected old credential. Unscoped admin only. Issue the replacement through uplink add first; no key material crosses MCP.", object(map[string]any{"identity": stringProperty("Uplink identity"), "fingerprint": stringProperty("Old key fingerprint"), "replacement_fingerprint": stringProperty("Already issued replacement key fingerprint")}, "identity", "fingerprint", "replacement_fingerprint")),
		tool("access_dismiss", "Dismiss one stale pending declaration. Unscoped admin only. Copy its exact repository key from access_list; legacy or orphaned keys are supported. Scoped uplinks are refused. Audited; later admission can observe it again.", object(map[string]any{"repo": stringProperty("Repository"), "step": stringProperty("Declared step"), "secret": stringProperty("Exact declared path")}, "repo", "step", "secret")),
		tool("kubevirt_grant_list", "List active KubeVirt grants for every repository. Unscoped admin only.", object(map[string]any{})),
		tool("access_list", "List secret access grants for a repository.", object(map[string]any{"repo": stringProperty("Repository name (empty lists all)"), "revoked": map[string]any{"type": "boolean", "description": "Include revoked grants"}, "pending": map[string]any{"type": "boolean", "description": "Return declared-but-not-granted paths observed at Workflow admission (issue #623)"}})),
		tool("access_allow", "Grant a step access to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "step": stringProperty("Step/template name"), "secret": stringProperty("Exact secret path from the pipeline declaration or access_list (e.g. oberth/data/release/cosign-secret). Copied verbatim; no prefix expansion or path normalization.")}, "repo", "step", "secret")),
		tool("access_revoke", "Revoke a step's access to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "step": stringProperty("Step/template name"), "secret": stringProperty("Exact secret path from the pipeline declaration or access_list (e.g. oberth/data/release/cosign-secret). Copied verbatim; no prefix expansion or path normalization.")}, "repo", "step", "secret")),
		tool("kubevirt_grant", "Grant a registered repository permission to submit narrowly admitted KubeVirt VMI templates and KVM device leaves (oberth.ci/kvm-templates, including guest-test lanes), with a VMI budget shared across its admitted runs. The budget bounds VMI creates only; KVM leaves declare no VMI demand. Unset budget fields default to 2 VMs, 6 cores, 12Gi; the server's cluster ceiling still caps the budget at admission. Granting an active grant again with a different budget replaces it. Admin uplink required; audited.", object(map[string]any{
			"repo":       stringProperty("Registered repository name"),
			"max_vms":    integerProperty("Maximum concurrent VMIs across admitted runs, 1..16 (default 2)"),
			"max_cores":  integerProperty("Maximum concurrent VMI cores across admitted runs, 1..64 (default 6)"),
			"max_memory": stringProperty("Maximum concurrent VMI memory requests across admitted runs as a Kubernetes quantity, 1Gi..256Gi (default 12Gi)"),
		}, "repo")),
		tool("kubevirt_revoke", "Revoke a repository's KubeVirt permission for VMI templates and KVM device leaves. New submissions fail closed; running workflows continue. Admin uplink required; audited.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("kubevirt_grant_get", "Read a repository's KubeVirt grant, which authorizes both VMI templates and KVM device leaves (oberth.ci/kvm-templates), and its VMI budget shared across admitted runs.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("size_grant", "Raise (or set) the largest admission size a repository's branch, promotion and schedule runs may declare. The default without a grant is L; XL reserves the whole host and makes every other repository wait for a full drain. An active grant (including L) also grants highest tag/release queue priority. Admin uplink required; audited.", object(map[string]any{"repo": stringProperty("Registered repository name"), "max": map[string]any{"type": "string", "enum": []string{"L", "XL"}, "description": "Maximum size"}}, "repo", "max")),
		tool("size_revoke", "Revoke a repository's size grant and automatic tag/release queue priority, returning it to the default maximum L. Admin uplink required; audited.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("size_grant_get", "Read a repository's maximum admission size and its size grant record.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("queue_priority_config", "Replace administrator RC branch globs (case-sensitive * and ? match slash); [] disables RC priority. Applies to waiting runs immediately. Normal and high runs age into highest priority after 60 minutes. Unscoped admin only; audited.", object(map[string]any{"rc_patterns": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16}}, "rc_patterns")),
		tool("queue_priority_get", "Read administrator RC globs and starvation threshold. Unscoped admin only.", object(map[string]any{})),
		tool("run_priority", "Bump one run awaiting resource admission to high or highest priority. Cannot lower priority or preempt admitted work. Unscoped admin only; audited, reason required.", object(map[string]any{"run_id": stringProperty("Exact run ID"), "priority": map[string]any{"type": "string", "enum": []string{"high", "highest"}}, "reason": stringProperty("Reason for the bump, 1..512 bytes")}, "run_id", "priority", "reason")),
		tool("admission_release", "Release one admission reservation no owner will release: typically a granted reservation still held for a run that is already terminal because its owner hung or was lost (also an ungranted one of a terminal run, or a retained one while a stopping scheduler drains). A live run's reservation is refused (use run_cancel). The run's Workflow is deleted first as proof; a granted run with no recorded Workflow (a trusted preflight) needs force, as does a failed deletion. Admin uplink required, unscoped only; audited. system_status lists every reservation.", object(map[string]any{"run_id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters) holding the reservation"), "force": map[string]any{"type": "boolean", "description": "Release even if the Workflow cannot be proven gone"}}, "run_id")),
		tool("repo_list", "List registered repositories with their upstream and probe state.", object(map[string]any{})),
		tool("repo_archive", "Archive a repository, retaining all history and refusing new work. Requires an unscoped admin; refuses in-flight runs and pending promotions. Drops the Git cache.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("repo_unarchive", "Unarchive a repository and allow new work again. Requires an unscoped admin.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("repo_remove", "Remove a repository mapping and its Git cache. Requires admin uplink. Refuses if runs are in-flight or promotions are pending.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("run_list", "List the whole bounded queue first in position order, then admitted and terminal runs by admission time, newest first. A full dated page returns next_before (or next_cursor for a scoped uplink); pass it as before with the same filters for older runs until it is absent.", object(map[string]any{
			"repo":   stringProperty("Filter by repository name"),
			"ref":    stringProperty("Filter by branch or tag ref"),
			"limit":  integerProperty("Maximum dated runs per page (default 50, max 100); the first page also carries the whole bounded queue"),
			"before": map[string]any{"type": []string{"integer", "string"}, "description": "Exclusive cursor: next_before (a run sequence) or next_cursor (a run ID) from the previous page"},
			"view": map[string]any{"type": "string", "enum": []string{"all", "active", "history"}, "default": "all",
				"description": "all: queue first, then admitted/terminal runs by admission time (default). history: terminal runs only with legacy paging. active: deprecated compatibility view of non-terminal runs in scheduling order, unpaged and capped at 500"},
		})),
		tool("audit_list", "List verified audit-chain entries newest first. Admin uplink required; a scoped uplink gets no audit rows. Each entry carries its chain position (id, sha256, previous_sha256) and is recomputed against the chain's hash before it is returned. A full page returns next_before; pass it as before for older entries.", object(map[string]any{
			"action": stringProperty("Action prefix, e.g. backup. or secret_access."),
			"actor":  stringProperty("Exact actor identity"),
			"repo":   stringProperty("Repository name: rows about it, its runs and its promotions"),
			"since":  stringProperty("RFC 3339 time, or a duration such as 24h meaning that long ago"),
			"until":  stringProperty("RFC 3339 time (exclusive), or a duration meaning that long ago"),
			"before": map[string]any{"type": "integer", "minimum": 0, "description": "Exclusive audit ID cursor (next_before from the previous page)"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50)"},
		})),
		tool("schedule_list", "List the schedules declared in repositories' .oberth/schedule.yaml with their cron, next run, last outcome and any pause. A scoped uplink sees its own repositories.", object(map[string]any{"repo": stringProperty("Repository name; omit to list every visible repository with a schedule")})),
		tool("schedule_pause", "Pause one schedule entry: a due tick records outcome paused instead of firing. Requires admin uplink; audited as schedule.pause.", object(map[string]any{"repo": stringProperty("Repository name"), "entry": stringProperty("Schedule entry name"), "reason": stringProperty("Why it is paused (recorded in the audit chain)")}, "repo", "entry")),
		tool("schedule_resume", "Resume a paused schedule entry. It fires once at its next tick if it came due while paused. Requires admin uplink; audited as schedule.resume.", object(map[string]any{"repo": stringProperty("Repository name"), "entry": stringProperty("Schedule entry name")}, "repo", "entry")),
		tool("schedule_trigger", "Run one schedule entry now: queues a trigger=schedule run at the entry's branch head through the normal CI admission path, as the caller. Refused while the repository has active runs, or the entry is paused, unless force is set (a forced run supersedes active runs of the same branch and entry, while other entries and ordinary pushes remain independent). Requires admin uplink; audited as schedule.trigger.", object(map[string]any{"repo": stringProperty("Repository name"), "entry": stringProperty("Schedule entry name"), "force": map[string]any{"type": "boolean", "description": "Run despite active runs in the repository or a paused entry"}}, "repo", "entry")),
		tool("system_status", "Return system health: database, upstreams, cluster, audit, and version.", object(map[string]any{})),
		tool("publish_retry", "Retry the upstream publication for a branch or tag run whose burns all passed but whose upstream push failed. Input is one full run ID or unique hexadecimal prefix of at least 8 characters. Refuses when the run is not in a publishing-failed state, or when the cached Git object no longer matches the record. A pending response names the publication and run IDs; poll run_get or wait for the run while finalization is recovered. A promotion (or its CI run) is retried with promote_retry instead, which merges onto the current target head and re-tests.", object(map[string]any{
			"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters) whose publication failed"),
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
		tool("promote_grant", "Create a time-bounded promotion grant authorizing any non-admin uplink to promote an exact (repository, SHA, target) tuple. Admin uplink required. The grant is single-use: consumed atomically when a matching promotion succeeds.", object(map[string]any{
			"repo":    stringProperty("Repository name"),
			"sha":     stringProperty("Full 40-character source commit SHA"),
			"branch":  stringProperty("Target branch"),
			"expires": stringProperty("Expiry as RFC3339 timestamp or Go duration (e.g. 24h, 1h30m); defaults to the configured maximum lifetime"),
			"note":    stringProperty("Optional free-text note for audit context"),
		}, "repo", "sha", "branch")),
		tool("promote_grant_revoke", "Revoke an active promotion grant. Admin uplink required. Already-consumed or already-revoked grants are refused.", object(map[string]any{
			"id": stringProperty("Grant ID to revoke"),
		}, "id")),
		tool("promote_grant_list", "List promotion grants with optional repository, SHA, and status filters. Read-only.", object(map[string]any{
			"repo":   stringProperty("Filter by exact repository name"),
			"sha":    stringProperty("Filter by exact full source SHA"),
			"status": map[string]any{"type": "string", "enum": []string{"active", "revoked", "expired", "consumed", "all"}, "description": "Filter by grant status (default: all)"},
		})),
	}
}

// IssueReadFields is the public MCP field allowlist; persistence-only fields
// cannot be selected. Keep the schema and service validation on this list.
var IssueReadFields = []string{
	"id", "lock", "upstream", "kind", "title", "body", "state", "repo_id",
	"branch", "occurrences", "ci_origin", "ci_work_id", "created_at", "updated_at",
}

func issueReadProperties(key string, value any) map[string]any {
	return map[string]any{
		key: value,
		"fields": map[string]any{
			"type": "array", "minItems": 1, "uniqueItems": true,
			"items":       map[string]any{"type": "string", "enum": IssueReadFields},
			"description": "Return only these issue fields; omit for the full record",
		},
		"body_limit": map[string]any{
			"type": "integer", "minimum": 0,
			"description": "Maximum retained UTF-8 body bytes, including occurrence history; marker added outside limit. Zero returns only a truncation marker for a nonempty body. Omit for unlimited body",
		},
	}
}
