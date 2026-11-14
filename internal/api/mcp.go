package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Prefer the CLI's version while retaining the original HTTP compatibility
// version. Supporting a new date requires an explicit compatibility review.
const mcpProtocolVersion = "2025-06-18"

func supportedMCPProtocol(version string) bool {
	return version == "2025-03-26" || version == mcpProtocolVersion
}

func negotiateMCPProtocol(raw json.RawMessage) (string, error) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", errors.New("initialize requires a protocolVersion date string")
	}
	if _, err := time.Parse(time.DateOnly, params.ProtocolVersion); err != nil {
		return "", errors.New("initialize requires a protocolVersion date string")
	}
	if supportedMCPProtocol(params.ProtocolVersion) {
		return params.ProtocolVersion, nil
	}
	// MCP negotiation returns a supported alternative. The client must inspect
	// the result and disconnect if it cannot use that version.
	return mcpProtocolVersion, nil
}

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
	// PruneSource is "pending" when prune_source was recorded (#1514); the
	// outcome arrives in promote_status and the promotion.finished event.
	PruneSource string `json:"prune_source,omitempty"`
	// TagAfter is "pending" when tag_after was recorded (#1524); the outcome
	// arrives in promote_status tag_after and the promotion.finished event.
	TagAfter string `json:"tag_after,omitempty"`
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
	BranchCISkipped bool   `json:"branch_ci_skipped"`
	SourceCount     int    `json:"source_count"`
	Repo            string `json:"repo"`
	Ref             string `json:"ref"`
	ID              string `json:"id"`
	Sequence        int64  `json:"sequence"`
	RepoID          int64  `json:"repo_id"`
	SourceBranch    string `json:"source_branch"`
	SourceSHA       string `json:"source_sha"`
	TargetRef       string `json:"target_ref"`
	ResultSHA       string `json:"result_sha,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}
type PromotionListResponse struct {
	Promotions []PromotionListItem `json:"promotions"`
	NextBefore int64               `json:"next_before,omitempty"`
	// NextCursor replaces NextBefore for a scoped uplink: the oldest
	// promotion's ID, never a global sequence (#073).
	NextCursor string `json:"next_cursor,omitempty"`
}

// PublishRetryResponse reports the outcome of retrying a failed upstream
// publication. PublicationID identifies the durable intent reactivated for the
// retry. Status is the publication status; RunStatus/RunPhase identify the run
// observation returned by run_get. A pending publication means delivery/finalization
// is still being recovered;
// poll run_get or wait using the returned run ID for its eventual outcome.
type PublishRetryResponse struct {
	// Action names the admitted work. Status retains its legacy meaning;
	// RunStatus and RunPhase describe the same durable observation as run_get.
	Action    string `json:"action"`
	Message   string `json:"message"`
	RunStatus string `json:"run_status"`
	RunPhase  string `json:"run_phase"`

	RunID         string `json:"run_id,omitempty"`
	PromotionID   string `json:"promotion_id,omitempty"`
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

// PublicationAbandonResponse reports an explicit administrative terminal verdict.
type PublicationAbandonResponse struct {
	Action        string `json:"action"`
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	RunID         string `json:"run_id,omitempty"`
	RunStatus     string `json:"run_status,omitempty"`
	RunPhase      string `json:"run_phase,omitempty"`
	PromotionID   string `json:"promotion_id,omitempty"`
	AbandonedBy   string `json:"abandoned_by"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
}

// RunCancelResponse reports durable cancellation intent. A submitted run remains
// active until Argo Stop finishes its exit handlers. Workflow is none (never
// claimed), not_submitted (no Workflow yet) or pending (Stop awaits completion).
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

	// Action names the admitted work. Status retains its legacy meaning;
	// RunStatus and RunPhase describe the same durable observation as run_get.
	Action    string `json:"action"`
	Message   string `json:"message"`
	RunStatus string `json:"run_status"`
	RunPhase  string `json:"run_phase"`

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
	WorkStatus  string           `json:"work_status"`
	Stale       bool             `json:"stale"`
	Revision    int64            `json:"revision"`
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
	Revision      int64  `json:"revision"`
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
	WorkStatus  string           `json:"work_status"`
	Stale       bool             `json:"stale"`
	Revision    int64            `json:"revision"`
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
	Workflow   string  `json:"workflow"`
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
	Workflow    string `json:"workflow"`
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
	// ArchivedHidden counts archived repositories omitted by the default
	// archived=exclude filter, so a caller knows the catalog has more (#1498).
	ArchivedHidden int `json:"archived_hidden,omitempty"`
}

func (server *Server) handleMCP(writer http.ResponseWriter, request *http.Request) {
	// MaxBytesReader enforces the body limit at the HTTP layer, returning a
	// 413 and closing the connection on oversize bodies rather than allowing a
	// trickle-body to hold a goroutine indefinitely. The server's ReadTimeout
	// bounds the time dimension; this bounds the byte dimension.
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	var incoming rpcRequest
	if err := decodeOne(request.Body, &incoming); err != nil {
		if writeOversizedBody(writer, err) {
			return
		}
		writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	if incoming.JSONRPC != "2.0" || incoming.Method == "" {
		writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: normalizeID(incoming.ID), Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	// This endpoint has no session state. An initialize body's version is
	// authoritative; subsequent requests carry their own selected version.
	// Headerless direct callers retain the original 2025-03-26 behavior.
	versions := request.Header.Values("MCP-Protocol-Version")
	if len(versions) > 1 || (len(versions) == 1 && !supportedMCPProtocol(versions[0])) {
		http.Error(writer, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	if len(incoming.ID) == 0 {
		if incoming.Method == "initialize" {
			http.Error(writer, "initialize requires a request ID", http.StatusBadRequest)
			return
		}
		// notifications/initialized acknowledges the client-side handshake;
		// it neither invokes tools nor changes another HTTP request's state.
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	response := rpcResponse{JSONRPC: "2.0", ID: incoming.ID}
	owner := sessionOwner(actorFrom(request.Context()))
	session, sessionOK := server.sessions.lookup(request, owner, incoming.Method == "resources/subscribe" || (incoming.Method == "tools/call" && strings.Contains(string(incoming.Params), "events_subscribe")))
	if !sessionOK {
		http.Error(writer, "unknown MCP session", http.StatusNotFound)
		return
	}
	switch incoming.Method {
	case "initialize":
		version, err := negotiateMCPProtocol(incoming.Params)
		if err != nil {
			response.Error = &rpcError{Code: -32602, Message: err.Error()}
			break
		}
		capabilities := map[string]any{"tools": map[string]any{}}
		if server.pushAvailable() {
			// Server push (#1514): the session-push capability Claude Code
			// accepts, and standard run resource subscriptions.
			capabilities["experimental"] = map[string]any{"claude/channel": map[string]any{}}
			capabilities["resources"] = map[string]any{"subscribe": true, "listChanged": false}
			writer.Header().Set(mcpSessionHeader, server.sessions.create("", owner).id)
		}
		response.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    capabilities,
			"serverInfo":      map[string]string{"name": "oberth", "version": server.version},
			"instructions":    "Record fields are snake_case (id, repo_id, steps). Blocking tools take their bound as timeout_s. Use status/wait/logs for SHA-oriented CI, run_get/run_logs for exact promotion run IDs, sync only to park the selected exact WIP branch upstream without a green gate, and promote to merge, test, and push a target branch. Sync is not completion evidence. Prefer issue_get/issue_get_many with fields/body_limit for routine status reads; run_get returns the summary by default and the full record with summary:false. Filter logs with pattern/context/limit/tail rather than retrieving a whole step. Log reads scan bounded byte windows; counts are lower bounds unless scan_complete is true. When has_more is true, continue with next_byte_offset as byte_offset, offset=0 and the same tail/pattern/context, including after an empty window. offset skips only within the current scanned window; with tail that window can change during lookback. An empty incomplete page does not prove absence. Authenticated JSON dashboard state is available at /api/runs, /api/repos, /api/issues, /api/grants, and /api/status.",
		}
		if server.pushAvailable() {
			result := response.Result.(map[string]any)
			result["instructions"] = result["instructions"].(string) + agentLoopInstructions
		}
	case "notifications/initialized":
		response.Error = &rpcError{Code: -32600, Message: "notifications/initialized must not have a request ID"}
	case "ping":
		response.Result = map[string]any{}
	case "resources/list", "resources/templates/list", "resources/read", "resources/subscribe", "resources/unsubscribe":
		if !server.pushAvailable() {
			response.Error = &rpcError{Code: -32601, Message: "method not found"}
			break
		}
		server.handleMCPResources(request, session, incoming.Method, incoming.Params, &response)
	case "tools/list":
		response.Result = map[string]any{"tools": toolDefinitionsFor(actorFrom(request.Context()))}
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
		if longPollTool(call.Name) {
			server.handleMCPLongPoll(writer, request, response, call)
			return
		}
		if bound := toolWriteBound(call.Name); bound > 0 {
			// A bounded tool (run_hold_exec, run_hold_patch) may outlast the
			// server-wide WriteTimeout; its reply must still be written.
			_ = http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(bound + writeDeadlineMargin))
		}
		result, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), call.Name, call.Arguments)
		if subscribed, ok := result.(EventsSubscribeResponse); ok && err == nil && session != nil {
			server.sessions.setSubscription(session, subscribed.Subscription)
			subscribed.Session = true
			subscribed.Push = "GET stream of MCP session " + session.id + ": notifications/claude/channel and notifications/resources/updated"
			result = subscribed
		}
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

// longPollTool names the tools served through the long-poll transport:
// heartbeats while waiting, a write deadline past the tool's own bound.
func longPollTool(name string) bool {
	switch name {
	case "wait", "promote_status", "run_wait", "events_wait":
		return true
	}
	return false
}

func (server *Server) pushAvailable() bool {
	feed, ok := server.tools.(EventFeed)
	return ok && feed.AgentEventWake() != nil
}

// agentLoopInstructions extend the initialize instructions when the server
// records agent events. The protocol is one open events_wait, which works in
// every client; whether a client also displays pushed events is its own
// choice (Claude Code needs a per-session flag), and event ids make such a
// copy harmless.
const agentLoopInstructions = " Agent loop: while this session has runs in flight, keep one events_wait {kinds:[run.held,run.finished], own:true, after_id, timeout_s:600} open (in the background when the client offers that for long tool calls) instead of polling. Act on every returned event (run.held: service the hold before abandon_at; run.finished: read status and retry_hint), then call it again with next_after_id as after_id, also after timed_out; stop when nothing is in flight. Without after_id it waits for the next event only, so start it before the push or check the run once with run_get. Events carry an increasing id: skip any id at or below the highest one handled, which makes a pushed copy harmless (some interactive sessions also receive notifications/claude/channel for run.finished, run.held, run.lock_wait, promotion.finished, release.published and publication.failed; events_subscribe narrows those; never wait for them). run_wait blocks on one run (timeout_s up to 600), and events_since {after_id} reads what happened in order after a gap or reconnect. run_get retry_hint says whether a failure is infrastructure (retry unchanged) or code."

// handleMCPSessionDelete ends a session (streamable-HTTP DELETE).
func (server *Server) handleMCPSessionDelete(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.sessions.lookup(request, sessionOwner(actorFrom(request.Context())), false)
	if !ok || session == nil {
		http.Error(writer, "unknown MCP session", http.StatusNotFound)
		return
	}
	server.sessions.remove(session)
	writer.WriteHeader(http.StatusNoContent)
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
	raw, err := marshalRecords(payload)
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
	// Admission outcome is explicitly public and independent of the private
	// error cause, including causes replaced by an internal-error reference.
	var outcome interface{ MCPWorkOutcome() string }
	if errors.As(err, &outcome) {
		if public := outcome.MCPWorkOutcome(); public != "" && !strings.HasPrefix(message, public+": ") {
			message = public + ": " + message
		}
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

var registeredToolNames = sync.OnceValue(func() map[string]bool {
	names := map[string]bool{}
	for _, definition := range toolDefinitions() {
		names[definition["name"].(string)] = true
	}
	return names
})

func knownTool(name string) bool { return registeredToolNames()[name] }

// adminOnlyTools refuse every non-admin uplink whatever the arguments; the
// service enforces that (TestAdminOnlyToolsRefuseNonAdmins proves this set
// against it). tools/list leaves them out for a non-admin token, so an agent
// is not offered 46 tools it can only be refused; a call is still answered
// with the server's refusal.
var adminOnlyTools = map[string]bool{
	"uplink_add":   true,
	"uplink_scope": true,
	"upstream_add": true, "upstream_list": true, "upstream_remove": true, "upstream_provide_key": true,
	"repo_add": true, "repo_verify": true, "admin_access_list": true, "admin_public_identity": true,
	"admin_readiness": true, "admin_version": true, "admin_idle": true, "admin_audit_record": true,
	"publication_abandon": true, "run_purge": true, "issue_unlock": true, "issue_close_many": true,
	"uplink_invite": true, "uplink_cert": true, "uplink_list": true, "uplink_revoke": true, "uplink_rotate": true,
	"access_dismiss": true, "access_allow": true, "access_revoke": true,
	"workflow_capability_grant": true, "workflow_capability_revoke": true,
	"release_wif_list": true, "release_wif_grant": true, "release_wif_revoke": true, "release_wif_reset": true,
	"repo_archive": true, "repo_unarchive": true, "repo_remove": true,
	"branch_delete_many": true, "branch_deletion_list": true, "branch_deletion_resolve": true, "ref_delete_many": true, "receive_outbox": true,
	"audit_list": true, "schedule_pause": true, "schedule_resume": true, "schedule_trigger": true,
	"secretstore_plan": true, "secretstore_sync_receipt": true, "secretstore_verify": true,
	"promote_grant": true, "promote_grant_revoke": true,
}

// AdminOnlyTool reports whether tools/list shows name only to admin uplinks.
func AdminOnlyTool(name string) bool { return adminOnlyTools[name] }

var nonAdminToolDefinitions = sync.OnceValue(func() []map[string]any {
	definitions := toolDefinitions()
	visible := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		if !AdminOnlyTool(definition["name"].(string)) {
			visible = append(visible, definition)
		}
	}
	return visible
})

// toolDefinitionsFor is tools/list for one caller: every tool for an admin,
// the tools a non-admin can use otherwise.
func toolDefinitionsFor(actor Actor) []map[string]any {
	if actor.Admin {
		return toolDefinitions()
	}
	return nonAdminToolDefinitions()
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

// maxAssistArgv bounds run_hold_exec argv entries (service.maxAssistExecArgs).
const maxAssistArgv = 64

// maxAssistExecSeconds is argojob.MaxAssistExecTimeout in seconds: the
// longest run_hold_exec any installation can configure.
const maxAssistExecSeconds = 900

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
	longPollTimeout := func() map[string]any {
		return map[string]any{"type": "integer", "description": "Wait bound in seconds; omitted or zero defaults to 600; values above 600 are rejected", "minimum": 0, "maximum": 600, "default": 600}
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		if repo, ok := properties["repo"].(map[string]any); ok {
			repo["description"] = fmt.Sprint(repo["description"]) + "; use upstream/org/name when ambiguous (for example codeberg/acme/widget). Ambiguity errors list fully qualified visible candidates."
		}
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema, "outputSchema": toolOutputSchema(name)}
	}
	return append(append(administrationToolDefinitions(tool, object, stringProperty), []map[string]any{
		tool("status", "Return concise CI status for a full SHA, short SHA, or branch, including the failed step.", object(map[string]any{"repo": stringProperty("Repository name when the selector is ambiguous"), "ref": stringProperty("Full SHA, short SHA, or branch")}, "ref")),
		tool("logs", "Read a retained native container for a SHA with node_id/container, list bindings with containers:true, or read historical run bytes with raw:true. Step selects an Argo node ID or a unique display name; historical step names remain readable. Optionally filter server-side. Prefer a pattern over retrieving a whole step: counts are lower bounds unless scan_complete is true; continue has_more pages with next_byte_offset as byte_offset and offset:0.", object(logFilterProperties(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "node_id": stringProperty("Native Argo node ID"), "container": stringProperty("Native Pod container name"), "containers": map[string]any{"type": "boolean", "description": "List native node/container bindings"}, "raw": map[string]any{"type": "boolean", "description": "Read historical run bytes without marker interpretation"}, "step": stringProperty("Argo node ID or unique step display name")}), "sha")),
		tool("run_get", "Return one run by full durable ID or unique hexadecimal prefix (at least 8 characters). Default (summary): id/repo/ref/sha, status/phase/failed_burn/failed_step/error, display_status/display_reason and compact Argo nodes (node_id, display_name, template, phase, message, pod_name, burn, name, status, started_at, finished_at). display_status is one of queued, running, passed, failed or cancelled; display_reason names the cause. Node display names, burns, step names and messages are sanitized and limited to 512 characters; passed nodes omit messages. Steps are paged (default/max 100): continue next_step_offset as step_offset. Set summary:false for the full record: step results with diagnostics, versioned retained Workflow/step timing without admission episodes (missing timing is unknown; unavailable=pruned once the run is older than the retained timing window), the repository record and Scheduling.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "summary": map[string]any{"type": "boolean", "description": "true (default) returns run identity, run context, failure and compact steps; false returns the full record"}, "step_offset": map[string]any{"type": "integer", "minimum": 0, "description": "Step offset from next_step_offset (default 0; summary only)"}, "step_limit": map[string]any{"type": "integer", "minimum": 0, "maximum": 100, "description": "Steps per summary page (default/max 100; 0 uses default)"}}, "id")),
		tool("publication_abandon", "Abandon one pending upstream publication by its full publication ID. Requires an unscoped admin uplink and a reason. Atomically marks the publication abandoned and its run failed/abandoned (and its promotion failed), with the administrator in the audit chain. Never pushes; earlier pushes may already be visible upstream. Abandoned intents cannot be retried with publish_retry. An in-flight delivery settles before this operation; terminal publications are refused.", object(map[string]any{
			"id":     stringProperty("Full publication ID from publication logs, publish_retry, or audit_list with action publication.pending"),
			"reason": stringProperty("Why publication is abandoned; required printable text, at most 512 bytes"),
		}, "id", "reason")),
		tool("run_cancel", "Cancel one queued or running run (preparation displays running). A claimed run stays active with a cancelling reason until native Argo Stop and its exit handlers finish; A stopped Workflow that did not succeed finishes cancelled; native success stays passed. Accepted work with no Workflow finishes cancelled. A second cancel escalates an active Stop to native Terminate, skipping exit handlers. Release/tag/apply and promotion cancels use Terminate immediately. Non-admin uplinks may cancel only branch-push runs; admins may cancel any run but must set force for tag/release and apply runs, whose publication may be left partial. Cancelling promotion CI fails the promotion without merging. Audited.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"reason": stringProperty("Why the run is cancelled (printable text, at most 512 bytes)"),
			"force":  map[string]any{"type": "boolean", "description": "Admin only: confirm cancelling a tag/release or apply run whose publication may be partial"},
		}, "id")),
		tool("run_rerun", "Rerun a failed or interrupted run at the same SHA and ref, linked to the original (rerun_of). Includes infrastructure failures and failed steps such as flaky tests. The source must be the newest branch run for its ref. SHA-keyed status, wait and promote then follow the newest run for that SHA. Refused while a tag has an active run, when a branch has moved past the run's SHA, and for promotion (use promote_retry), plan and apply runs. A run that passed every step and failed only in upstream publication is not re-executed: its publication is retried as publish_retry does (mode publish_retry, action publication_retry). Responses name the started action and include run_status/run_phase; no-work refusals say nothing started. Non-admin uplinks may rerun branch-push runs; tag/release runs need an admin; force-free replay requires the newest source, an unchanged tag object, complete native node history and recorded template/grant evidence for every tag attempt (or no Workflow intent). Started secret-store/WIF work, recursive or incomplete history, missing grant evidence, older sources, moved tags and unknown evidence require force. Pending Workflow Stop is refused even with force. A tag that is already published or green is refused, so it is never published twice. Audited as run.rerun.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"reason": stringProperty("Why the run is rerun (printable text, at most 512 bytes)"),
			"force":  map[string]any{"type": "boolean", "description": "Admin only: confirm replaying a tag/release run after secret access or publication work started or when replay safety is unknown"},
			"assist": map[string]any{"type": "boolean", "description": "Ask for agent-assisted continuation: a failed credential-free step of this branch CI rerun parks its retry for run_hold_* instead of failing (assist.enabled; ignored elsewhere)"},
		}, "id")),
		tool("run_hold_list", "List the parked retry attempts (holds) of an assisted branch CI run: step, pod, attempt, held_since, abandon_at (when the server abandons the hold unless someone acts: plan against this), kubelet deadline, whether run_hold_patch can apply files, and whether the hold was already continued or abandoned. Also returns the run's assist record and the installation policy (hold_ttl_s, exec_timeout_s in seconds). A run holds only when it asked for assistance (push option oberth.assist=1 or run_rerun assist), assist.enabled is on, and a credential-free step exited nonzero; the retry Pod parks before its command until run_continue, run_hold_abandon or assist.holdTTL (the server abandons an idle hold past the TTL; each exec, patch or continue restarts it, up to twice the TTL after held_since; the Pod deadline shown also covers the step's own budget after release). Read-only.", object(map[string]any{
			"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
		}, "id")),
		tool("run_hold_exec", "Run one bounded command (argv, no shell) inside the held step's main container, as an uplink whose scope covers the run's repository (who may push to it) or an admin: for diagnosing the failure in the live environment. Bounded by assist.execTimeout and assist.execOutputBytes (the deadline stops the stream; a process the image keeps running is not killed), serialized per run, capped installation-wide, and recorded in the run's assist record and audit (run.assist.exec) before the command runs. Any exec marks the run assisted: it is never evidence from this point, whatever the command did.", object(withTimeout(map[string]any{
			"id":    stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"step":  stringProperty("Held step (Argo template name); optional when exactly one step is held"),
			"argv":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": maxAssistArgv, "description": "Command and arguments, executed directly (argv[0] must be on PATH or absolute); no shell interpretation"},
			"stdin": stringProperty("Optional standard input, at most 1 MiB"),
		}, map[string]any{"type": "integer", "minimum": 1, "maximum": maxAssistExecSeconds, "description": "Optional shorter deadline in seconds than assist.execTimeout (itself at most 900)"}), "id", "argv")),
		tool("run_hold_patch", "Apply a unified diff to the held step's writable source tree (/work/src) through the server's own Pod helper: exact-context hunks, relative cleaned paths only, no renames, binaries, mode changes, .git or symlink traversal, bounded by assist.patchMaxBytes/patchMaxFiles. The diff's sha256 is recorded in the run's assist record and audit (run.assist.patch) before it is applied, and the run is assisted from then on: never promotion, release or green-for-SHA evidence; push the change as a commit for a real green run.", object(map[string]any{
			"id":   stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"step": stringProperty("Held step (Argo template name); optional when exactly one step is held"),
			"diff": stringProperty("Unified diff (git diff or diff -u output)"),
		}, "id", "diff")),
		tool("run_continue", "Release a held step: the parked executor runs the step's real command on the (possibly patched) tree and Argo continues the DAG natively on success. Recorded before the release (run.assist.continue); the run is assisted (as it already is after any exec or patch) and its status carries an 'assisted: ... not evidence' reason line. An assisted run is never published upstream, never a promotion source and never green for its SHA, even when no patch or exec happened (a plain release is still not evidence). A release that fails abandons the Pod. Any uplink whose scope covers the run's repository, or an admin.", object(map[string]any{
			"id":   stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"step": stringProperty("Held step (Argo template name); optional when exactly one step is held"),
		}, "id")),
		tool("run_hold_abandon", "Delete a held step's parked Pod so the step fails normally and the run ends red as it would have without assistance. Recorded as run.assist.abandon. Any uplink whose scope covers the run's repository, or an admin.", object(map[string]any{
			"id":   stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"step": stringProperty("Held step (Argo template name); optional when exactly one step is held"),
		}, "id")),
		tool("run_purge", "Purge retained logs, artifacts, or both for one terminal run. Requires an unscoped admin uplink and a reason. Active runs, release evidence, promotion evidence, open-issue evidence and active grants are protected. Run and step history stays visible; every purge is audited.", object(map[string]any{
			"id":     stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"what":   map[string]any{"type": "string", "enum": []string{"logs", "artifacts", "both"}},
			"reason": stringProperty("Why evidence is purged; required printable text, at most 512 bytes"),
		}, "id", "what", "reason")),
		tool("artifacts", "List the files a run kept: test reports, coverage output, anything the pipeline copied to OBERTH_ARTIFACTS. Cheap; call this before artifact_get.", object(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)")}, "id")),
		tool("artifact_get", "Return one artifact's contents by run ID and name, optionally filtered server-side. Prefer a pattern over retrieving a whole file: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one.", object(logFilterProperties(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "name": stringProperty("Artifact name as reported by artifacts")}), "id", "name")),
		tool("run_logs", "Read a retained container with node_id and container, or list containers with containers:true. Container bytes are plain Kubernetes logs, including any marker-looking text. Native log selection resolves a step to its Argo node ID; unique display names are accepted for discovery. Historical logs keep their stored burn/step ranges. Select by full run ID or unique hexadecimal prefix (at least 8 characters), optionally filtered server-side. With no pattern, limit, tail, offset or byte_offset, returns the last 200 lines and a window explanation. Pass tail:true with limit:N for a larger tail, or limit:0 for the existing bounded full read. Explicit filters retain their behavior. Without burn or step, failed runs select the failed step; passed runs return the first container node in run order plus available_pairs (burn/step strings). A step without burn selects its unique matching burn; ambiguous names return the candidate burns. Active steps without retained output return an actionable pending error. Log discovery/filtering scan at most 4 MiB each per call and stop head pages early on limit. total_scanned_bytes includes both phases; scan_limit_bytes is the aggregate cap (4 MiB terminal, 8 MiB active). Pattern scans allow one concurrent call per bearer token and two globally; waiting shares the 10-second read budget. When has_more is true, continue with next_byte_offset as byte_offset, retaining tail/pattern/context and resetting offset to zero. Counts are lower bounds unless scan_complete is true; bytes is exact only when bytes_exact is true. withheld_bytes/withheld_lines report omitted data. Lines above 64 KiB are omitted and counted.", object(logFilterProperties(map[string]any{"raw": map[string]any{"type": "boolean", "description": "Read historical run bytes as plain text without marker interpretation"}, "containers": map[string]any{"type": "boolean", "description": "List observed native node IDs and container names instead of reading bytes"}, "node_id": stringProperty("Native Argo node ID for a retained container copy"), "container": stringProperty("Native container name"), "id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"), "burn": stringProperty("Burn name (inferred from an explicit unique step; otherwise defaults to the failed burn)"), "step": stringProperty("Argo node ID or unique display name (defaults to the failed node or the selected burn's only container node)")}), "id")),
		tool("wait", "Long-poll until a SHA or exact run reaches a terminal state; timeout returns still-running cleanly. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline. Pass id alone to wait for one run by full ID or unique prefix of at least 8 hex characters. When trigger is set, waits for a run with that trigger (e.g. 'release' for a tag-push run).", object(withTimeout(map[string]any{"id": stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters); cannot be combined with repo, sha, or trigger"), "repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA (required unless id is provided)"), "trigger": stringProperty("Filter by trigger type (e.g. 'release' for tag runs, 'branch' for branch runs)")}, longPollTimeout()))),
		tool("sync", "Park the exact SHA's WIP branch upstream without a green gate; this is not completion or promotion evidence. Rejects promotion, release, plan, and apply runs. When the SHA has branch-trigger runs on multiple distinct branches, the explicit branch argument is required.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Explicit branch name; required when the SHA has runs on multiple branches, optional otherwise")}, "sha")),
		tool("promote", "Green-gate and publish without force. Provide sha for one source, or 1..32 ordered sources for a merge train: sources must be exactly tested green branch commits or authorized park refs. Parked sources always run full merged-tree CI, with skipped branch CI recorded. Trains wait in the existing target queue, merge onto its current head and run CI once before publishing. Conflicts name source and paths; skip_conflicting drops them and records the reason. Example: {\"repo\":\"codeberg/acme/widgets\",\"branch\":\"main\",\"sources\":[{\"sha\":\"1111111111111111111111111111111111111111\"},{\"sha\":\"2222222222222222222222222222222222222222\"},{\"sha\":\"3333333333333333333333333333333333333333\"}]}", promotionInputSchema()),
		tool("promotion_list", "Recover durable promotion IDs by exact repository/source SHA/status. Read-only, newest admission first; pass next_before (or next_cursor for a scoped uplink) as before for older records. Status comes from the committed promotion row, as in promote_status; remote ref equality alone never marks a pending record passed.", object(map[string]any{
			"repo":       stringProperty("Filter by exact repository name"),
			"source_sha": stringProperty("Filter by full source commit SHA; no ref or prefix resolution"),
			"status":     map[string]any{"type": "string", "enum": []string{"pending", "passed", "failed", "interrupted"}, "description": "Filter by current promotion status"},
			"before":     map[string]any{"type": []string{"integer", "string"}, "description": "Exclusive cursor: next_before (an admission sequence) or next_cursor (a full promotion ID or unique hexadecimal prefix of at least 8 characters) from the previous page"},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50, maximum 200)"},
		})),
		tool("promote_status", "Wait for an append-only promotion record to become terminal. A passed promotion includes promotion.observed_target_sha, the exact forge target read with the server deploy key before completion, equal to ResultSHA. A failed or interrupted promotion that was retried names its successor in retried_as. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline.", object(withTimeout(map[string]any{"id": stringProperty("Full promotion ID or unique hexadecimal prefix (at least 8 characters)")}, longPollTimeout()), "id")),
		tool("promote_retry", "Retry a failed or interrupted promotion: a new promotion of the same repository, source SHA and target, merged onto the target's current head and re-tested like promote. After a transient push-only failure, an unchanged target reuses the exact tested SHA without repeating CI. Same authorization as promote; a grant is consumed only by a delivered promotion, so the original grant authorizes the retry. Refused while a promotion of the same tuple is pending or delivered, or when the source SHA's newest run is not green. Audited as promotion.retry.", object(map[string]any{"id": stringProperty("Full ID or unique hexadecimal prefix (at least 8 characters) of the failed or interrupted promotion")}, "id")),
		tool("issue_create", "Create a manual issue, installation-wide or filed under one upstream. Uplinks scoped to that upstream see it; an uplink scoped to one upstream files there by default.", object(map[string]any{"title": stringProperty("Issue title"), "body": stringProperty("Issue body"), "upstream": stringProperty("Upstream name to file the issue under (required for an uplink scoped to several upstreams)")}, "title", "body")),
		tool("issue_get", "Get an issue by ID. Full record by default for compatibility. Optional fields selects only named fields; body_limit caps retained body/occurrence-history bytes with an explicit truncated N bytes marker. The numeric occurrences count remains exact. Read-only; does not renew locks.", object(issueReadProperties("id", integerProperty("Issue ID")), "id")),
		tool("issue_get_many", "Get 1..50 issues in request order, each with issue or error (not_found or response_limit). Full records by default. Optional fields and body_limit apply to each issue before the 256 KiB structured JSON budget; body_limit truncates body/occurrence history with a truncated N bytes marker, keeping numeric occurrences exact. Use compact options or issue_get for response_limit IDs. Read-only; does not renew locks.", object(issueReadProperties("ids", map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1}}), "ids")),
		tool("issue_update", "Update work_status (todo, active, waiting, review), title and body, or move a manual issue to an upstream (empty or global means installation-wide, admin only). A non-admin can move only a visible issue to an upstream in scope. expected_revision atomically checks the current metadata revision; a mismatch is a conflict. Revision advances only for real title, state, work_status or upstream changes, not body edits. When expected_body_sha256 is set, the server checks the existing body hash inside the write transaction and rejects a mismatch. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID"), "title": stringProperty("Issue title (body optional)"), "body": stringProperty("Replacement issue body (title optional)"), "work_status": issueWorkStatusProperty(), "upstream": stringProperty("New upstream name, or empty/global for installation-wide (admin only)"), "expected_revision": map[string]any{"type": "integer", "minimum": 1, "description": "Optional current metadata revision; mismatch is a conflict; omitted means last-write-wins"}, "expected_body_sha256": stringProperty("Hex-encoded SHA-256 of the existing body; the server rejects the update on mismatch")}, "id")),
		tool("issue_append", "Append text to an issue body with a dated separator, without replacing the existing content. Use for CI investigation updates without re-sending the existing body. Returns only id, state, updated_at, occurrences, appended_bytes (UTF-8 bytes of text, excluding separator); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID"), "text": stringProperty("Text to append")}, "id", "text")),
		tool("issue_close", "Close an issue without deleting its history. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_reopen", "Reopen a manual issue without changing its ID or history. Already-open manual issues are unchanged; CI issues cannot be reopened manually.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_delete", "Delete an accidentally created manual issue.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_list", "List issue metadata, newest creation ID first (not latest updated), with total and faceted state/kind counts. Filters apply before pagination; counts exclude the cursor. Returns work_status and computed stale for open issues past their window. Defaults to all issues, 50 per page; pass next_before as before with the same filters until absent for a complete queue scan.", object(map[string]any{
			"repo":               stringProperty("Exact repository name; global manual issues have no repository"),
			"work_status":        issueWorkStatusProperty(),
			"stale_after":        stringProperty("Positive duration overriding the general stale window (server default 168h)"),
			"active_stale_after": stringProperty("Positive duration overriding the active stale window (server default 1h); must not exceed stale_after"),
			"kind":               map[string]any{"type": "string", "enum": []string{"manual", "ci"}},
			"state":              map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}},
			"query":              stringProperty("Case-insensitive substring in issue title, body or branch"),
			"before":             map[string]any{"type": "integer", "minimum": 0, "description": "Exclusive issue ID cursor; zero starts the first page"},
			"limit":              map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Page size (default 50)"},
		})),
		// Issue lifecycle administration (#1103).
		tool("issue_unlock", "Break an issue lock, recording its previous owner. Admin only; visibility and audit gate enforced.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_close_many", "Close 1..50 unique issues with a reason and per-ID results. Admin only; active locks remain enforced, one audit action per closure.", object(map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}, "minItems": 1, "maxItems": 50, "uniqueItems": true}, "reason": stringProperty("Nonempty closure reason, at most 4096 bytes")}, "ids", "reason")),
		tool("issue_lock", "Acquire or renew the caller-owned five-minute issue lock.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		// Credential and grant lifecycle (#1105).
		tool("uplink_invite", "Create a single-use, expiring certificate enrollment invitation. Unscoped admin only. Supply the SHA-256 digest of a random 32-byte credential generated locally; no secret is returned. Use oberth uplink invite to write the private handoff file.", object(map[string]any{"identity": stringProperty("New identity@host"), "server_url": stringProperty("Public HTTPS /mcp URL"), "secret_digest": stringProperty("SHA-256 digest of a locally generated random 32-byte credential"), "scope": map[string]any{"type": "array", "items": stringProperty("Upstream name")}, "expires": stringProperty("Invite lifetime, default 24h, maximum 168h")}, "identity", "server_url", "secret_digest")),
		tool("uplink_cert", "Issue a client certificate for an existing uplink from a locally generated CSR. Unscoped admin only. EC P-256 or Ed25519; default 2160h, bounded to 1h–8760h. Returns only the public certificate.", object(map[string]any{"identity": stringProperty("Uplink identity"), "fingerprint": stringProperty("SSH fingerprint to disambiguate"), "csr": stringProperty("PEM certificate request"), "ttl": stringProperty("Certificate lifetime")}, "identity", "csr")),
		tool("client_renew", "Renew the authenticated uplink client certificate using a new local CSR. Requires current certificate authentication. Old and new pins overlap; private keys stay local.", object(map[string]any{"csr": stringProperty("PEM certificate request"), "ttl": stringProperty("Certificate lifetime")}, "csr")),
		tool("uplink_list", "List public uplink identities and usage. Unscoped admin only. No tokens are returned.", object(map[string]any{})),
		tool("uplink_revoke", "Revoke an uplink SSH key and bearer immediately. Unscoped admin only; audited; the last active unscoped admin cannot be revoked.", object(map[string]any{"identity": stringProperty("Uplink identity"), "fingerprint": stringProperty("Exact key fingerprint, required when identity has several keys")}, "identity")),
		tool("uplink_rotate", "Finish rotation to an already registered active replacement key with identical identity, admin status and scope, atomically revoking the selected old credential. Unscoped admin only. Issue the replacement through uplink add first; no key material crosses MCP.", object(map[string]any{"identity": stringProperty("Uplink identity"), "fingerprint": stringProperty("Old key fingerprint"), "replacement_fingerprint": stringProperty("Already issued replacement key fingerprint")}, "identity", "fingerprint", "replacement_fingerprint")),
		tool("access_dismiss", "Dismiss one stale pending declaration. Unscoped admin only. Copy its exact repository and workflow file from access_list; orphaned repository keys are supported. Scoped uplinks are refused. Audited; later admission can observe it again.", object(map[string]any{"repo": stringProperty("Repository"), "workflow": stringProperty("Exact repository-relative workflow file, e.g. .oberth/release.yaml; required, no wildcard or default"), "step": stringProperty("Declared step"), "secret": stringProperty("Exact declared path")}, "repo", "workflow", "step", "secret")),
		tool("access_list", "List secret access grants, including their exact workflow file, for a repository.", object(map[string]any{"repo": stringProperty("Repository name (empty lists all)"), "revoked": map[string]any{"type": "boolean", "description": "Include revoked grants"}, "pending": map[string]any{"type": "boolean", "description": "Return declared-but-not-granted paths observed at Workflow admission (issue #623)"}})),
		tool("access_allow", "Grant a step in an exact workflow file access to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "workflow": stringProperty("Exact repository-relative workflow file, e.g. .oberth/release.yaml; required, no wildcard or default"), "step": stringProperty("Step/template name"), "secret": stringProperty("Exact secret path from the pipeline declaration or access_list (e.g. oberth/data/release/cosign-secret). Copied verbatim; no prefix expansion or path normalization.")}, "repo", "workflow", "step", "secret")),
		tool("access_revoke", "Revoke a step's access in an exact workflow file to a secret path. Requires admin uplink.", object(map[string]any{"repo": stringProperty("Repository name"), "workflow": stringProperty("Exact repository-relative workflow file, e.g. .oberth/release.yaml; required, no wildcard or default"), "step": stringProperty("Step/template name"), "secret": stringProperty("Exact secret path from the pipeline declaration or access_list (e.g. oberth/data/release/cosign-secret). Copied verbatim; no prefix expansion or path normalization.")}, "repo", "workflow", "step", "secret")),
		tool("workflow_capability_grant", "Grant operator-owned Kubernetes API access to one registered repository. The operator owns the ServiceAccount, RBAC and namespace quota. Administrator only; audited. Existing technology-specific grants confer no authority here.", object(map[string]any{
			"repo": stringProperty("Registered repository name"),
			"capability": object(map[string]any{
				"namespace":       stringProperty("Configured execution namespace"),
				"service_account": stringProperty("Operator-provided ServiceAccount"),
				"resource_kinds": map[string]any{"type": "array", "maxItems": 32, "items": object(map[string]any{
					"api_version": stringProperty("Exact API version"), "kind": stringProperty("Exact resource kind"),
					"actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"create", "get", "delete", "apply"}}},
				}, "api_version", "kind", "actions")},
			}, "namespace", "service_account"),
		}, "repo", "capability")),
		tool("workflow_capability_revoke", "Revoke a repository's operator capability. New submissions and restart adoption refuse the revoked authority. Running Kubernetes workloads remain operator-owned. Administrator only; audited.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("workflow_capability_get", "Read the repository's current or revoked operator capability within the caller's upstream scope.", object(map[string]any{"repo": stringProperty("Registered repository name")}, "repo")),
		tool("release_wif_list", "Read the release WIF capability map: boot default from Helm/flags, runtime overrides, the effective map used by preflight and admission, configured role names and any stored grant left inactive. Public data only. Unscoped admin only.", object(map[string]any{})),
		tool("release_wif_grant", "Grant or replace one registered repository's release WIF entry at runtime (#1497): its release service_account_name and template-to-role map. Roles, namespace and dedicated step identities stay Helm configuration. Validated against the boot map; applies to the next preflight and tag admission without restart. Unscoped admin only; audited, reason required.", object(map[string]any{"repo": stringProperty("Registered repository (any accepted spelling; stored canonical upstream/org/repo)"), "service_account_name": stringProperty("The repository's per-repo release ServiceAccount"), "templates": map[string]any{"type": "object", "minProperties": 1, "maxProperties": 64, "additionalProperties": map[string]any{"type": "string"}, "description": "Pod leaf template name -> configured role"}, "reason": stringProperty("Reason, 1..512 bytes")}, "repo", "service_account_name", "templates", "reason")),
		tool("release_wif_revoke", "Revoke a repository's release WIF entry at runtime, including a Helm-configured entry or one for a retired, unregistered upstream/org/repo. New release admissions refuse its WIF leaves. Unscoped admin only; audited, reason required.", object(map[string]any{"repo": stringProperty("Repository or exact upstream/org/repo key"), "reason": stringProperty("Reason, 1..512 bytes")}, "repo", "reason")),
		tool("release_wif_reset", "Remove a repository's runtime release WIF override so the Helm boot default applies again. Unscoped admin only; audited, reason required.", object(map[string]any{"repo": stringProperty("Repository or exact upstream/org/repo key"), "reason": stringProperty("Reason, 1..512 bytes")}, "repo", "reason")),
		tool("repo_list", "List registered repositories as id, name, git_path (canonical SSH repository path), upstream, default_branch, archived_at, non_default_branches (mirror branches besides the default branch; null until counted) and one latest_run (id, status, ref, finished_at). Archived repositories are hidden by default and counted in archived_hidden; pass archived=include for the whole catalog or archived=only. No run history or probe payload.", object(map[string]any{
			"archived": map[string]any{"type": "string", "enum": []string{"exclude", "include", "only"}, "description": "Archived repository filter (default exclude)"},
		})),
		tool("repo_archive", "Archive a repository, retaining all history and refusing new work. Requires an unscoped admin; refuses in-flight runs and pending promotions. Drops the Git cache.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("repo_unarchive", "Unarchive a repository and allow new work again. Requires an unscoped admin.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("repo_remove", "Remove a repository mapping and its Git cache. Requires admin uplink. Refuses if runs are in-flight or promotions are pending.", object(map[string]any{"repo": stringProperty("Repository name")}, "repo")),
		tool("branch_delete_many", "Delete many mirror branches in one batch; upstream deletion follows asynchronously. Dry run by default: returns plan_id and per-branch head_sha, merged/patch_equivalent (git cherry), ahead/behind against the default branch head the forge last confirmed (not unpublished mirror commits) and last_commit; unmerged branches are listed separately. Select an explicit branches list of {branch, expected_sha}, or merged_into (the default branch) and/or older_than (30d, 72h) with optional exclude_prefixes. Executes only with confirm:true and the dry run's plan_id; any moved head makes the plan stale. Each ref is compared and deleted; unmerged branches need allow_unmerged and, to confirm, a reason that is recorded. The default branch is never deleted. Configured reserved branches are never selected; one may be deleted alone with confirm_reserved \"<upstream>/<org>/<repo>:<branch>\". Every plan entry has class merged, patch-equivalent, unique or unchecked (beyond the git cherry budget), and classes counts every candidate; report:true (dry run only) also lists kept candidates. Unscoped admin only; one audit entry per batch. Read per-branch upstream state with branch_deletion_list.", object(map[string]any{
			"repo":             stringProperty("Repository name"),
			"branches":         map[string]any{"type": "array", "maxItems": 1000, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"branch", "expected_sha"}, "properties": map[string]any{"branch": stringProperty("Branch name"), "expected_sha": stringProperty("Full head SHA the branch must still have")}}},
			"merged_into":      stringProperty("Selector: branches merged or patch-equivalent into this branch; must be the default branch"),
			"older_than":       stringProperty("Selector: last commit older than this age, e.g. 30d or 72h"),
			"exclude_prefixes": map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Selector: skip branches starting with any prefix, e.g. ci/, release/, park/"},
			"allow_unmerged":   map[string]any{"type": "boolean", "description": "Also delete branches that are neither merged nor patch-equivalent"},
			"confirm":          map[string]any{"type": "boolean", "description": "Execute the plan named by plan_id; omitted or false is a dry run"},
			"plan_id":          stringProperty("plan_id from the dry run with the same arguments"),
			"confirm_reserved": stringProperty("Typed confirmation <upstream>/<org>/<repo>:<branch> to delete one configured reserved (non-default) branch"),
			"report":           map[string]any{"type": "boolean", "description": "Dry run only: also list the selected branches that are kept (unique or unchecked) under kept, so the plan classifies every candidate"}, "reason": map[string]any{"type": "string", "maxLength": 512, "description": "Why unmerged branches are deleted; required to confirm a plan with unmerged entries (allow_unmerged) and recorded in the batch audit"},
		}, "repo")),
		tool("branch_deletion_list", "List accepted branch deletions newest first with their upstream state: pending (with attempts and retry_after), propagated, failed (upstream unchanged, mirror branch restored) or skipped (branch pushed again, head moved before a prune, upstream repository archived with the mirror deletion kept, or a failed record resolved; error holds the reason line). Filter by repo and status, or read one prune batch or receive event with batch. counts holds every record per status for repo (or all repositories) or for the whole batch; page_counts only the returned page. Unscoped admin only. A full page returns next_before.", object(map[string]any{
			"repo":   stringProperty("Repository name"),
			"status": map[string]any{"type": "string", "enum": []string{"pending", "propagated", "failed", "skipped"}},
			"batch":  stringProperty("Prune batch_id or receive event ID"),
			"before": map[string]any{"type": "integer", "minimum": 0, "description": "Exclusive cursor: next_before from the previous page"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50)"},
		})),
		tool("branch_deletion_resolve", "Close failed branch deletion records as skipped with a reason line; no ref changes (a failed deletion already restored its mirror branch and left upstream unchanged). With ids, resolves those failed records of repo. Without ids, resolves only failed records whose branch no longer exists on the mirror and lists the rest as kept. Failed records also resolve on their own when a later deletion of the same branch propagates, and hourly when their branch is gone from the mirror. Unscoped admin only; one audit entry per call.", object(map[string]any{
			"repo":   stringProperty("Repository name"),
			"ids":    map[string]any{"type": "array", "maxItems": 1000, "items": map[string]any{"type": "string"}, "description": "Failed record ids from branch_deletion_list; omit to resolve records whose branch is gone from the mirror"},
			"reason": stringProperty("Why the records are resolved, 1..512 bytes"),
		}, "repo", "reason")),
		tool("ref_delete_many", "Delete stray mirror refs outside refs/heads/ and refs/tags/, such as refs/remotes/upstream/* left by an old mirror tool; they are never mirrored, so the deletion is local and nothing changes upstream. refs/heads/, refs/tags/, refs/oberth/ and refs/replace/ are never listed or deleted. Dry run by default: without a selection it lists every stray ref (at most 5000) under kept; returns plan_id and per-ref sha, object_type, symref, last_commit and class default-branch, tag (reachable from a tag only), unreachable or not-commit, with classes counting every stray ref. Select an explicit refs list of {ref, expected_sha}, or prefixes such as refs/remotes/. Unreachable and not-commit refs need allow_unreachable. Executes only with confirm:true and the dry run's plan_id; any moved ref makes the plan stale. At most 500 refs per call; each is compared and deleted on its exact object ID, and a symbolic ref is deleted itself, never its target. Unscoped admin only; audited with every ref and object ID.", object(map[string]any{
			"repo":              stringProperty("Repository name"),
			"refs":              map[string]any{"type": "array", "maxItems": 500, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ref", "expected_sha"}, "properties": map[string]any{"ref": stringProperty("Full ref name, e.g. refs/remotes/upstream/main"), "expected_sha": stringProperty("Full object ID the ref must still name")}}},
			"prefixes":          map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Selector: stray refs starting with any prefix, e.g. refs/remotes/upstream/"},
			"allow_unreachable": map[string]any{"type": "boolean", "description": "Also delete refs not reachable from the default branch or a tag, or not naming a commit"},
			"confirm":           map[string]any{"type": "boolean", "description": "Execute the plan named by plan_id; omitted or false is a dry run"},
			"plan_id":           stringProperty("plan_id from the dry run with the same arguments"),
		}, "repo")),
		tool("receive_outbox", "Repair durable receive records that block a repository. list shows every pending or quarantined record; show reads one (leniently when corrupt); retry delivers it now (an operator-quarantined record is restored first when otherwise valid); abandon moves it aside to abandoned/ so the repository accepts pushes again. retry and abandon are audited; abandon needs reason and the record id from show. Unscoped admin only.", object(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "show", "retry", "abandon"}},
			"repo":   stringProperty("Record repository as listed (upstream/org/repo)"),
			"id":     stringProperty("Record id from show; required to abandon a readable record"),
			"reason": stringProperty("Why the record is abandoned, 1..512 bytes"),
		}, "action")),
		tool("run_list", "List preparing runs in acceptance order, then submitted runs newest start first, then terminal runs newest finish first. Optional repo/ref and status filters apply before paging; status accepts one display status or a list. Preparing and running runs are never dropped by the limit or pushed below finished runs. Preparation displays running; queued is derived only from Argo Pending. A full terminal page returns next_before (or next_cursor for a scoped uplink); pass it as before with the same filters for older runs until it is absent.", object(map[string]any{
			"repo":     stringProperty("Filter by repository name"),
			"status":   map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 10}}, "description": "Filter by one existing display status or a list: queued, running, passed, failed, cancelled. Paging is unchanged; repeat the same filters on later pages."},
			"ref":      stringProperty("Filter by branch or tag ref"),
			"ref_glob": stringProperty("Shell-style pattern on the ref, e.g. land/* (* never crosses a slash); empty: every ref"),
			"latest":   map[string]any{"type": "boolean", "description": "One row per matching repository and ref: its newest run, across every repository the caller's uplink scope reaches (all view only). Combine with ref_glob, e.g. {ref_glob:\"land/*\", latest:true}"},
			"own":      map[string]any{"type": "boolean", "description": "Only runs this uplink pushed or triggered (actor is the caller's identity); paging is unchanged"},
			"held":     map[string]any{"type": "boolean", "description": "Only running assisted runs with a parked retry attempt (hold) that no action released: one unpaged list, no before; service each with run_hold_list and run_hold_* before its abandon_at"},
			"limit":    integerProperty("Maximum terminal runs per page (default 50, max 100); the first page also carries every queued and running run"),
			"before":   map[string]any{"type": []string{"integer", "string"}, "description": "Exclusive cursor: next_before (a run sequence) or next_cursor (a run ID) from the previous page"},
			"view": map[string]any{"type": "string", "enum": []string{"all", "history"}, "default": "all",
				"description": "all: queue, then running runs, then terminal runs newest finish first (default). history: terminal runs only, paged by run sequence"},
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
		tool("schedule_trigger", "Run one schedule entry now: queues a trigger=schedule run at the entry's branch head through the normal CI admission path, as the caller. A paused entry requires force. Active runs do not delay submission. Requires admin uplink; audited as schedule.trigger.", object(map[string]any{"repo": stringProperty("Repository name"), "entry": stringProperty("Schedule entry name"), "force": map[string]any{"type": "boolean", "description": "Run a paused entry once"}}, "repo", "entry")),
		tool("system_status", "Return system health: database, upstreams, cluster, audit, and version.", object(map[string]any{})),
		tool("publish_retry", "Retry the upstream publication for a branch or tag run whose burns all passed but whose upstream push failed. Input is one full run ID or unique hexadecimal prefix of at least 8 characters. Refuses when the run is not in a publishing-failed or upstream publication infrastructure-failed state, or when the cached Git object no longer matches the record. Transient server/transport failures retry automatically up to five attempts over about thirty minutes. Responses name the actual action and include run_status/run_phase from the same committed snapshot as publication status. A pending response says publication is queued and names the publication and run IDs; poll run_get or wait for the run while finalization is recovered. A promotion (or its CI run) is retried with promote_retry instead, which reuses a tested push-only candidate when its target is unchanged, or merges onto the current head and re-tests.", object(map[string]any{
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
			"timeout_s":    map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "default": 45, "description": "Overall deadline in seconds, including TokenRequest and login"},
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
	}...), agentLoopToolDefinitions(tool, object, stringProperty)...)
}

// IssueReadFields is the public MCP field allowlist; persistence-only fields
// cannot be selected. Keep the schema and service validation on this list.
var IssueReadFields = []string{
	"revision",
	"id", "lock", "upstream", "kind", "title", "body", "state", "work_status", "stale", "repo_id",
	"branch", "occurrences", "ci_origin", "ci_work_id", "created_at", "updated_at",
}

func issueReadProperties(key string, value any) map[string]any {
	return map[string]any{
		key:                  value,
		"stale_after":        map[string]any{"type": "string", "description": "Positive duration overriding the general stale window"},
		"active_stale_after": map[string]any{"type": "string", "description": "Positive duration overriding the active stale window; must not exceed stale_after"},
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

func issueWorkStatusProperty() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"todo", "active", "waiting", "review"}, "description": "Work coordination status, independent of open/closed"}
}

func promotionInputSchema() map[string]any {
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"branch"},
		"properties": map[string]any{

			"repo":             map[string]any{"type": "string", "description": "Repository; required when a source SHA is ambiguous"},
			"sha":              map[string]any{"type": "string", "description": "Single full SHA; mutually exclusive with sources"},
			"branch":           map[string]any{"type": "string", "description": "Target branch name, e.g. main (not refs/heads/main)"},
			"sources":          map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "description": "Sources in merge order; optional ref must match the tested branch", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"sha"}, "properties": map[string]any{"sha": map[string]any{"type": "string"}, "ref": map[string]any{"type": "string"}}}},
			"skip_conflicting": map[string]any{"type": "boolean", "description": "Train only: drop conflicting sources and keep their paths in the promotion reason"},
			"prune_source":     map[string]any{"type": "boolean", "description": "Single source into the default branch only: after the promotion passed and the upstream target equals its result, delete the source branch (head still the promoted SHA) on the mirror and queue its upstream deletion. Any other outcome keeps the branch with a recorded reason, shown in promote_status prune_source and the promotion.finished event"},
			"tag_after": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "message"},
				"description": "After the promotion passed and the freshly checked upstream target contains its result, push an annotated tag (tagger Oberth) on the result commit, without force, with publication retries. The outcome (pushed or failed, with a reason) is in promote_status tag_after and the promotion.finished event. Admin uplinks only. The verified pushed tag starts ordinary tag CI through normal receive admission; it does not grant release authority. Release-version names (v<major>.<minor>.<patch> with any suffix) and administrator release routes are refused; push those through release CI. Refused when the tag already exists",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string", "description": "Tag name without refs/tags/, e.g. deploy/2026-11-11"},
					"message": map[string]any{"type": "string", "description": "Annotated tag message, 1..4096 bytes"},
				}},
		},
	}
	return schema
}

// withTimeout adds a blocking tool's own bound as timeout_s, the one
// spelling every blocking tool takes.
func withTimeout(properties map[string]any, bound map[string]any) map[string]any {
	properties[ToolTimeoutArgument] = bound
	return properties
}

// agentLoopToolDefinitions are the read-only MCP agent loop tools (#1514).
func agentLoopToolDefinitions(tool func(string, string, map[string]any) map[string]any, object func(map[string]any, ...string) map[string]any, stringProperty func(string) map[string]any) []map[string]any {
	return []map[string]any{
		tool("run_wait", "Block server-side until one run finishes, holds a retry (assisted runs: a parked step waits for run_hold_*), or changes in any way, bounded by timeout_s (default and maximum 600). Returns state finished, held, changed or timeout with the run summary, its unreleased holds and retry_hint. Read-only; replaces client polling loops.", object(withTimeout(map[string]any{
			"id":    stringProperty("Full run ID or unique hexadecimal prefix (at least 8 characters)"),
			"until": map[string]any{"type": "string", "enum": []string{"finished", "held", "any"}, "description": "finished (default): terminal status; held: a parked retry hold or terminal; any: any change of status, phase, failed step, error, assist record or holds"},
		}, map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "Wait bound in seconds (default 600)"}), "id")),
		tool("events_subscribe", "Set which agent events this MCP session receives as server push: a notifications/claude/channel message on the session's GET event stream (Claude Code shows it only in an interactive session started with --dangerously-load-development-channels server:<name>; otherwise use run_wait and events_since), and notifications/resources/updated for run://<id> resources the session subscribed. Kinds: run.finished (status, failed step, reason, retry_hint), run.held (step, pod, held_since, abandon_at, holds_in_run), run.hold_abandoned (step, pod, reason ttl, cap or the agent, held_since, abandoned_at, failed attempt and exit code), run.lock_wait (a native Argo lock wait past agentEvents.lockWaitThreshold, with the lock and its holders; lock Parallelism/workflow-controller with postponed true is a Workflow the Argo controller postponed for its parallelism), promotion.finished (with the prune_source and tag_after outcomes), release.published and publication.failed (final, or an automatic retry attempt). Without a call a session receives every kind for every repository in its uplink scope. Events carry a monotonically increasing id; delivery is at-least-once; after a reconnect resume with events_since. Read-only.", object(map[string]any{
			"repos":     map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Repositories (qualified or unique names) in scope; empty: all"},
			"refs_glob": stringProperty("Shell-style pattern on the ref, e.g. land/*; empty: all"),
			"kinds":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"run.finished", "run.held", "run.hold_abandoned", "run.lock_wait", "promotion.finished", "release.published", "publication.failed"}}, "description": "Event kinds; empty: all"},
			"own":       map[string]any{"type": "boolean", "description": "Only events about runs and promotions of this uplink"},
		})),
		tool("events_since", "Read agent events after a cursor in id order, scoped to the caller's uplink scope and the same optional filters as events_subscribe. Use after a reconnect or without a push stream: pass next_after_id back as after_id. gap is true when older events were pruned (agentEvents.retention). Read-only.", object(map[string]any{
			"after_id":  map[string]any{"type": "integer", "minimum": 0, "description": "Return events with a larger id; 0 starts at the oldest retained"},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size before filtering (default 100)"},
			"repos":     map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Repositories (qualified or unique names) in scope; empty: all"},
			"refs_glob": stringProperty("Shell-style pattern on the ref, e.g. land/*; empty: all"),
			"kinds":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"run.finished", "run.held", "run.hold_abandoned", "run.lock_wait", "promotion.finished", "release.published", "publication.failed"}}, "description": "Event kinds; empty: all"},
			"own":       map[string]any{"type": "boolean", "description": "Only events about runs and promotions of this uplink"},
		})),
		tool("events_wait", "Block server-side until at least one agent event after after_id passes every filter (the caller's uplink scope, repos, refs_glob, kinds, own, run_ids), or until timeout_s (default and maximum 600). Returns the events_since page of the first match; pass next_after_id back as after_id for the next wait, so one call covers every run and every hold of a worker. On timeout: no events, timed_out true and next_after_id moved past every event scanned. Without after_id it waits for the next event. run_ids match run events only. Read-only.", object(withTimeout(map[string]any{
			"after_id":  map[string]any{"type": "integer", "minimum": 0, "description": "Wait for events with a larger id; omitted: only events recorded after the call"},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size before filtering (default 100)"},
			"run_ids":   map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Only events about these runs (full IDs or unique prefixes of at least 8 characters); empty: all"},
			"repos":     map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}, "description": "Repositories (qualified or unique names) in scope; empty: all"},
			"refs_glob": stringProperty("Shell-style pattern on the ref, e.g. land/*; empty: all"),
			"kinds":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"run.finished", "run.held", "run.hold_abandoned", "run.lock_wait", "promotion.finished", "release.published", "publication.failed"}}, "description": "Event kinds; empty: all"},
			"own":       map[string]any{"type": "boolean", "description": "Only events about runs and promotions of this uplink"},
		}, map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "Wait bound in seconds (default 600)"}))),
		tool("sync_status", "Native Argo synchronization: the current holders and waiters of every semaphore and mutex observed on active runs (sampled every agentEvents.interval from Workflow status.synchronization), and of the Argo controller parallelism as Parallelism/workflow-controller (waiters are postponed Workflows, holders admitted ones), and the bounded stored history per lock: holder and waiter intervals, the maximum number of concurrent holders (proves two runs never overlapped) and wait durations. A scoped uplink sees only its own runs. Read-only.", object(map[string]any{
			"lock":  stringProperty("One lock key, e.g. ConfigMap/oberth-sync-limits/kvm or Mutex/oberth-release; empty: all"),
			"since": stringProperty("History window such as 6h or 7d (default 24h, maximum 30d)"),
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "description": "Maximum stored intervals, newest first (default 2000)"},
		})),
	}
}

func administrationToolDefinitions(tool func(string, string, map[string]any) map[string]any, object func(map[string]any, ...string) map[string]any, stringProperty func(string) map[string]any) []map[string]any {
	return []map[string]any{
		tool("uplink_add", "Enroll an uplink under an existing unscoped admin. Public SSH key and optional CSR only; private keys stay local. CSR enrollment mints no bearer unless requested. Administrator grants are explicit; audited without credential material.", object(map[string]any{"identity": stringProperty("Uplink identity@host"), "ssh_public_key": stringProperty("One bare SSH public key, never a path"), "csr": stringProperty("Optional public PEM CSR"), "admin": map[string]any{"type": "boolean"}, "bearer": map[string]any{"type": "boolean"}, "ttl": stringProperty("Credential lifetime, bounded by server policy"), "scope": map[string]any{"type": "array", "maxItems": 128, "items": stringProperty("Upstream name or metrics")}}, "identity", "ssh_public_key")),
		tool("uplink_scope", "Set an uplink repository scope; an empty array clears it. Unscoped admin only; audited. Requires an existing admin identity.", object(map[string]any{"identity": stringProperty("Uplink identity@host"), "fingerprint": stringProperty("Optional SHA256 SSH public-key fingerprint to disambiguate"), "scope": map[string]any{"type": "array", "maxItems": 128, "items": stringProperty("Registered upstream name or metrics")}}, "identity", "scope")),
		tool("upstream_add", "Register an upstream using the server identity store. Unscoped admin only; audited. Requires an existing admin identity. SSH bootstrap uses pinned host keys and fixed identity bundles.", object(map[string]any{
			"name": stringProperty("Upstream name"), "base_url": stringProperty("SSH repository namespace URL; local filesystem paths are refused"),
			"dedicated_key": map[string]any{"type": "boolean"}, "expected_host_fingerprint": stringProperty("Expected SHA256 SSH host fingerprint"),
			"repin": map[string]any{"type": "boolean"}, "check_repo": stringProperty("Repository name to probe"), "no_wait": map[string]any{"type": "boolean"},
		}, "name", "base_url")),
		tool("upstream_list", "List registered upstreams, optionally checking authentication. Unscoped admin only; audited.", object(map[string]any{"check": map[string]any{"type": "boolean"}})),
		tool("upstream_remove", "Remove an upstream and its repository mappings. Unscoped admin only; audited.", object(map[string]any{"name": stringProperty("Upstream name")}, "name")),
		tool("upstream_provide_key", "Import an SSH private key into the fixed upstream identity bundle. Unscoped admin only; audited without key material. Never accepts a key file path.", object(map[string]any{"name": stringProperty("Upstream name"), "private_key": stringProperty("Unencrypted SSH private key material, at most 64 KiB"), "dedicated_key": map[string]any{"type": "boolean"}}, "name", "private_key")),
		tool("repo_add", "Register a repository mapping; the first push confirms its default branch. Unscoped admin only; audited.", object(map[string]any{"repo": stringProperty("Relative repository identity"), "upstream": stringProperty("Registered upstream name"), "default_branch": stringProperty("Default branch, main when omitted")}, "repo", "upstream")),
		tool("repo_verify", "Probe a registered repository using the server's existing upstream identity. Unscoped admin only; audited.", object(map[string]any{"repo": stringProperty("Relative repository identity")}, "repo")),
		tool("admin_access_list", "Read all active grants for installer identity production. Unscoped admin only; audited. Returns path names only.", object(map[string]any{})),
		tool("admin_public_identity", "Read named public identity material. Unscoped admin only; audited. Never accepts a filesystem path.", object(map[string]any{"name": map[string]any{"type": "string", "enum": []string{"server-certificate", "server-ca", "upstream-public-key"}}}, "name")),
		tool("admin_readiness", "Read current readiness with a safe reason code. Unscoped admin only; audited.", object(map[string]any{})),
		tool("admin_version", "Read server binary and database schema versions. Unscoped admin only; audited.", object(map[string]any{})),
		tool("admin_idle", "Observe the all or critical queue; the installer owns waiting. Unscoped admin only; audited. This observation never freezes admission.", object(map[string]any{"wait_for": map[string]any{"type": "string", "enum": []string{"all", "critical"}}})),
		tool("admin_audit_record", "Record a completed operator lifecycle action. Unscoped admin only; actor identity comes from authentication.", object(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"backup.completed", "restore.completed", "upgrade.completed"}}, "details": map[string]any{"type": "object"}}, "action")),
	}
}
