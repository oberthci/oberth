package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// AdminRequestBytes also bounds direct MCP calls to administrative tools.
const AdminRequestBytes = 128 << 10

// UpstreamAddRequest carries configuration, never a local path or command.
type UpstreamAddRequest struct {
	Name                    string `json:"name"`
	BaseURL                 string `json:"base_url"`
	DedicatedKey            bool   `json:"dedicated_key,omitempty"`
	ExpectedHostFingerprint string `json:"expected_host_fingerprint,omitempty"`
	Repin                   bool   `json:"repin,omitempty"`
	CheckRepository         string `json:"check_repo,omitempty"`
	NoWait                  bool   `json:"no_wait,omitempty"`
}

type UpstreamKeyRequest struct {
	Name         string `json:"name"`
	DedicatedKey bool   `json:"dedicated_key,omitempty"`
	// #nosec G117 -- Request-only key import over authenticated TLS; never returned or audited.
	PrivateKey string `json:"private_key"`
}

// UpstreamIdentity contains only public onboarding results.
type UpstreamIdentity struct {
	PublicKey       string `json:"public_key"`
	Fingerprint     string `json:"fingerprint"`
	Generated       bool   `json:"generated"`
	Pending         bool   `json:"pending"`
	RestartRequired bool   `json:"restart_required"`
}

type RepoAddRequest struct {
	Repo          string `json:"repo"`
	Upstream      string `json:"upstream"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// adminRoutes is shared by registration and the client action inventory.
var adminRoutes = map[string]string{
	"uplink-scope": "uplink_scope",
	"upstream-add": "upstream_add", "upstream-list": "upstream_list",
	"upstream-remove": "upstream_remove", "upstream-provide-key": "upstream_provide_key",
	"repo-add": "repo_add", "repo-verify": "repo_verify",
	"access-list": "admin_access_list", "public-identity": "admin_public_identity",
	"readiness": "admin_readiness", "version": "admin_version",
	"idle": "admin_idle", "audit-record": "admin_audit_record",
	"database-backup": "admin_database_backup",
}

func (s *Server) administrationRoutes() {
	for action, name := range adminRoutes {
		s.mux.Handle("POST /api/admin/"+action, s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleAdministration(w, r, name)
		})))
	}
}

func (s *Server) handleAdministration(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Cache-Control", "no-store")
	actor := actorFrom(r.Context())
	if !actor.Admin || actor.Scoped() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "administration requires an unscoped admin identity"})
		return
	}
	if r.URL.RawQuery != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "administration accepts JSON arguments only"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, AdminRequestBytes))
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "administration body too large"})
		return
	}
	if err != nil || ValidateAdminObject(body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "administration requires one JSON object"})
		return
	}
	if name == "admin_database_backup" {
		backend, ok := s.tools.(interface {
			PrepareAdminBackup(context.Context, Actor, json.RawMessage) (io.ReadCloser, error)
		})
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database backup unavailable"})
			return
		}
		archive, err := backend.PrepareAdminBackup(r.Context(), actor, body)
		if err != nil {
			s.writeView(w, nil, err)
			return
		}
		defer func() { _ = archive.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="oberth-database-backup.tar"`)
		w.Header().Set("Cache-Control", "no-store")
		if _, err := io.Copy(w, archive); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	value, err := s.tools.CallTool(r.Context(), actor, name, body)
	s.writeView(w, value, err)
}
