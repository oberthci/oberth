package goproxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

type modulePseudoVersionSnapshot interface {
	ModulePseudoVersionSnapshot(context.Context, string, string, string, bool) (string, time.Time, error)
}
type moduleSubdirPseudoVersionSnapshot interface {
	ModuleSubdirPseudoVersionSnapshot(context.Context, string, string, string, string, bool) (string, time.Time, error)
}
type pseudoSnapshot struct {
	source    string
	timestamp time.Time
}

// WithPseudoVersionsOnNonDefaultBranches expands the default-branch-only
// policy to other public branches and tags already allowed by the module proxy.
// This administrator setting must be configured before serving requests.
func (h *Handler) WithPseudoVersionsOnNonDefaultBranches(allow bool) *Handler {
	h.pseudoNonDefault = allow
	return h
}

func (h *Handler) pseudoSnapshot(ctx context.Context, repository, modulePath, version string) (pseudoSnapshot, error) {
	location, _ := h.moduleLocation(modulePath)
	var resolve func(context.Context) (string, time.Time, error)
	if location.subdir != "" {
		backend, ok := h.cache.(moduleSubdirPseudoVersionSnapshot)
		if !ok {
			return pseudoSnapshot{}, backendBuildError()
		}
		resolve = func(ctx context.Context) (string, time.Time, error) {
			return backend.ModuleSubdirPseudoVersionSnapshot(ctx, repository, modulePath, location.subdir, version, h.pseudoNonDefault)
		}
	} else {
		backend, ok := h.cache.(modulePseudoVersionSnapshot)
		if !ok {
			return pseudoSnapshot{}, backendBuildError()
		}
		resolve = func(ctx context.Context) (string, time.Time, error) {
			return backend.ModulePseudoVersionSnapshot(ctx, repository, modulePath, version, h.pseudoNonDefault)
		}
	}
	return retryModuleBuild(ctx, h.backendBudget, func(attempt context.Context) (pseudoSnapshot, error) {
		source, timestamp, err := resolve(attempt)
		if errors.Is(err, gitcache.ErrModulePseudoVersionUnavailable) {
			return pseudoSnapshot{}, pseudoUnavailableError()
		}
		if err != nil {
			return pseudoSnapshot{}, backendBuildError()
		}
		if gitcache.ValidateSHA(source) != nil {
			return pseudoSnapshot{}, backendBuildError()
		}
		return pseudoSnapshot{source, timestamp}, nil
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
