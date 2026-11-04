package goproxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/oberthci/oberth/internal/gitcache"
)

// WithPseudoVersionsOnNonDefaultBranches expands the default-branch-only
// policy to other public branches and tags already allowed by the module proxy.
// This administrator setting must be configured before serving requests.
func (h *Handler) WithPseudoVersionsOnNonDefaultBranches(allow bool) *Handler {
	h.pseudoNonDefault = allow
	return h
}

func (h *Handler) pseudoSnapshot(ctx context.Context, repository, modulePath, version string) (gitcache.ModuleSnapshot, error) {
	location, _ := h.moduleLocation(modulePath)
	backend, ok := h.cache.(moduleVersionSnapshot)
	if !ok {
		return gitcache.ModuleSnapshot{}, backendBuildError()
	}
	return retryModuleBuild(ctx, h.backendBudget, func(attempt context.Context) (gitcache.ModuleSnapshot, error) {
		snapshot, err := backend.ModuleVersionSnapshot(attempt, repository, modulePath, location.subdir, version, h.pseudoNonDefault)
		if errors.Is(err, gitcache.ErrModulePseudoVersionUnavailable) {
			return gitcache.ModuleSnapshot{}, pseudoUnavailableError()
		}
		if err != nil || gitcache.ValidateSHA(snapshot.Commit) != nil {
			return gitcache.ModuleSnapshot{}, backendBuildError()
		}
		return snapshot, nil
	})
}
func pseudoUnavailableError() *proxyBuildError {
	return &proxyBuildError{class: "pseudo_version_unavailable", status: http.StatusForbidden}
}
func pseudoUnavailable(w http.ResponseWriter) {
	// Private refusals must stop comma-separated GOPROXY fallback, including
	// pseudo-version coordinates that contain private commit prefixes.
	w.Header().Set("Cache-Control", "no-store")
	proxyError(w, "pseudo_version_unavailable", "pseudo-version unavailable: commit must exist, have the exact timestamp and base, and be reachable from an allowed ref", http.StatusForbidden)
}
