package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (server *Server) handleAudit(writer http.ResponseWriter, request *http.Request) {
	actor := actorFrom(request.Context())
	if !actor.Admin || actor.Scoped() {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "audit_list requires an unscoped admin uplink"})
		return
	}
	query := request.URL.Query()
	arguments := map[string]any{}
	for _, name := range []string{"action", "actor", "repo", "since", "until"} {
		if value := query.Get(name); value != "" {
			arguments[name] = value
		}
	}
	for _, name := range []string{"before", "limit"} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid audit " + name})
				return
			}
			arguments[name] = value
		}
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	value, err := server.tools.CallTool(request.Context(), actor, "audit_list", raw)
	if page, ok := value.(interface{ AuditNextBefore() int64 }); ok && err == nil && page.AuditNextBefore() > 0 {
		writer.Header().Set("X-Oberth-Next-Before", strconv.FormatInt(page.AuditNextBefore(), 10))
	}
	server.writeView(writer, value, err)
}
