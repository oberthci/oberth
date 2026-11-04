package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (server *Server) handleAdmissionHold(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	if err != nil {
		code := http.StatusBadRequest
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(writer, code, map[string]string{"error": "cannot read admission hold arguments (maximum 1 MiB)"})
		return
	}
	// REST uses exactly the MCP authorization, scope, mutation gate and audit path.
	value, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), "admission_hold", json.RawMessage(body))
	if err != nil {
		code, message := server.classifyError(err)
		writeJSON(writer, code, map[string]string{"error": message})
		return
	}
	writeJSON(writer, http.StatusOK, value)
}
