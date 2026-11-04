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
	backend, ok := h.cache.(modulePseudoVersionSnapshot)
	if !ok {
		return pseudoSnapshot{}, backendBuildError()
	}
	return retryModuleBuild(ctx, h.backendBudget, func(attempt context.Context) (pseudoSnapshot, error) {
		source, timestamp, err := backend.ModulePseudoVersionSnapshot(attempt, repository, modulePath, version, h.pseudoNonDefault)
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
	return &proxyBuildError{class: "pseudo_version_unavailable", status: http.StatusGone}
}
func pseudoUnavailable(w http.ResponseWriter) {
	// 410 is an explicit pseudo-version refusal, not a cached absence. Go may
	// try the next configured proxy; ordinary private misses retain 403.
	w.Header().Set("Cache-Control", "no-store")
	proxyError(w, "pseudo_version_unavailable", "pseudo-version unavailable: commit must exist, have the exact timestamp and base, and be reachable from an allowed ref", http.StatusGone)
}
