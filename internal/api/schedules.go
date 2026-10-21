package api

import (
	"encoding/json"
	"net/http"
)

func (server *Server) handleSchedules(writer http.ResponseWriter, request *http.Request) {
	raw, err := json.Marshal(map[string]string{"repo": request.URL.Query().Get("repo")})
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	value, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), "schedule_list", raw)
	server.writeView(writer, value, err)
}
