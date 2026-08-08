package api

import (
	"encoding/json"
	"errors"
	"net/http"
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
}

// PromoteResponse is intentionally only the durable promotion identifier.
// Promotion state is retrieved through promote_status, as the FAB contract
// requires, rather than exposing the internal persistence record.
type PromoteResponse struct {
	ID string `json:"id"`
}

// IssueCreateResponse is the complete FAB response for issue_create.
type IssueCreateResponse struct {
	ID int64 `json:"id"`
}

// IssueResponse is the complete issue record exposed to MCP clients. Internal
// projection sequence and storage ownership fields stay behind the service.
type IssueResponse struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	State string `json:"state"`
}

type IssueListItem struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

type IssueListResponse struct {
	Issues     []IssueListItem `json:"issues"`
	NextBefore int64           `json:"next_before,omitempty"`
}

type IssueLockResponse struct {
	ID        int64  `json:"id"`
	Owner     string `json:"owner"`
	ExpiresAt string `json:"expires_at"`
}

func (server *Server) handleMCP(writer http.ResponseWriter, request *http.Request) {
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
			"serverInfo":      map[string]string{"name": "oberth", "version": "dev"},
			"instructions":    "Use status/wait/logs for CI, sync only to park the selected exact WIP branch upstream without a green gate, and promote to merge, test, and push a target branch. Sync is not completion evidence. Authenticated JSON dashboard state is available at /api/runs, /api/repos, /api/issues, and /api/status.",
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
		result, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), call.Name, call.Arguments)
		if err != nil {
			response.Result = toolFailure(err)
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
	if value, ok := payload.(interface{ MCPToolText() string }); ok {
		return map[string]any{
			"content":           []map[string]string{{"type": "text", "text": value.MCPToolText()}},
			"structuredContent": payload,
			"isError":           false,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return toolFailure(err)
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}, "structuredContent": payload, "isError": false}
}

func toolFailure(err error) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
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
	timeoutProperty := func() map[string]any {
		return map[string]any{"type": "integer", "description": "Timeout seconds (maximum 120)", "minimum": 1, "maximum": 120}
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
		tool("logs", "Return exactly one named step log for a SHA.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "step": stringProperty("Step name")}, "sha", "step")),
		tool("wait", "Long-poll until a SHA reaches a terminal state; timeout returns still-running cleanly.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full or short SHA"), "timeout": timeoutProperty()}, "sha")),
		tool("sync", "Park the exact SHA's corresponding WIP branch upstream without a green gate; this is not completion or promotion evidence.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA")}, "sha")),
		tool("promote", "Green-gate a SHA, merge it with the target branch, prove the resulting tree when needed, and push without force.", object(map[string]any{"repo": stringProperty("Repository name when the SHA is ambiguous"), "sha": stringProperty("Full SHA"), "branch": stringProperty("Target branch")}, "sha", "branch")),
		tool("promote_status", "Wait for an append-only promotion record to become terminal.", object(map[string]any{"id": stringProperty("Promotion ID"), "timeout": timeoutProperty()}, "id")),
		tool("issue_create", "Create a global manual issue.", object(map[string]any{"title": stringProperty("Issue title"), "text": stringProperty("Issue body")}, "title", "text")),
		tool("issue_get", "Get an issue by ID.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_update", "Update an issue title and body.", object(map[string]any{"id": integerProperty("Issue ID"), "title": stringProperty("Issue title"), "body": stringProperty("Issue body")}, "id", "title", "body")),
		tool("issue_close", "Close an issue without deleting its history.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_delete", "Delete an accidentally created manual issue.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
		tool("issue_list", "List issue IDs and states in fixed pages of 50.", object(map[string]any{"before": integerProperty("Cursor issue ID")})),
		tool("issue_lock", "Acquire or renew the caller-owned five-minute issue lock.", object(map[string]any{"id": integerProperty("Issue ID")}, "id")),
	}
}
