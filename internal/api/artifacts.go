package api

import (
	"net/http"
	"path"
	"strings"

	"github.com/oberthci/oberth/internal/runlog"
)

type artifactBody interface {
	ArtifactBytes() []byte
	ArtifactName() string
}

func (server *Server) handleRunArtifacts(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "a run ID is required"})
		return
	}
	value, err := server.views.RunArtifacts(request.Context(), actorFrom(request.Context()), id)
	server.writeView(writer, value, err)
}

func (server *Server) handleRunArtifactDownload(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	name := request.PathValue("name")
	if !validRunSelector(id) || strings.TrimSpace(name) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "a run ID and artifact name are required"})
		return
	}
	value, err := server.views.RunArtifact(request.Context(), actorFrom(request.Context()), id, name, runlog.Filter{})
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	body, ok := value.(artifactBody)
	if !ok {
		server.writeView(writer, value, nil)
		return
	}
	writeArtifactHeaders(writer, name)
	// #nosec G705 -- served as application/octet-stream attachment with
	// X-Content-Type-Options: nosniff and CSP default-src 'none'; sandbox.
	// The response is a forced download, never rendered.
	_, _ = writer.Write(body.ArtifactBytes())
}

func writeArtifactHeaders(writer http.ResponseWriter, name string) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(name)+"\"")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	writer.Header().Set("Cache-Control", "no-store")
}
