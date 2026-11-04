package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/oberthci/oberth/internal/measured"
)

func (s *Server) handleRunResources(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "resource history unavailable"})
		return
	}
	q := r.URL.Query()
	last := 0
	failed := false
	var e error
	if q.Get("last") != "" {
		last, e = strconv.Atoi(q.Get("last"))
		if e != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid last"})
			return
		}
	}
	if q.Get("include_failed") != "" {
		failed, e = strconv.ParseBool(q.Get("include_failed"))
		if e != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid include_failed"})
			return
		}
	}
	resourceOnly := false
	if q.Get("resource_only") != "" {
		resourceOnly, e = strconv.ParseBool(q.Get("resource_only"))
		if e != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid resource_only"})
			return
		}
	}
	run := q.Get("run")
	alias := q.Get("run_id")
	if alias != "" && run != "" && alias != run {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conflicting run and run_id"})
		return
	}
	if run == "" {
		run = alias
	}
	if alias != "" && r.PathValue("run") != "" && alias != r.PathValue("run") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conflicting path and run_id"})
		return
	}
	if r.PathValue("run") != "" {
		run = r.PathValue("run")
	}
	raw, _ := json.Marshal(map[string]any{"repo": q.Get("repo"), "run": run, "ref": q.Get("ref"), "burn": q.Get("burn"), "step": q.Get("step"), "last": last, "include_failed": failed, "resource_only": resourceOnly, "format": q.Get("format")})
	value, e := s.tools.CallTool(r.Context(), actorFrom(r.Context()), "run_resources", raw)
	if e == nil && q.Get("format") == "yaml" {
		if report, ok := value.(measured.Report); ok {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = io.WriteString(w, measured.YAML(report))
			return
		}
	}
	s.writeView(w, value, e)
}
