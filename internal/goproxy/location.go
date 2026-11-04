package goproxy

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/oberthci/oberth/internal/gitcache"
)

type moduleLocation struct {
	repository string
	subdir     string
	major      string
}

// Only the first path component selects a mapped repository. Remaining
// components select a module inside that repository, never another upstream.
func (h *Handler) moduleLocation(modulePath string) (moduleLocation, bool) {
	namespace, _ := module.UnescapePath(h.modulePrefix)
	suffix, ok := strings.CutPrefix(modulePath, namespace+"/")
	if !ok || module.CheckPath(modulePath) != nil {
		return moduleLocation{}, false
	}
	root, nested, _ := strings.Cut(suffix, "/")
	repository, ok := h.repoForModule[root]
	if !ok || root == "" {
		return moduleLocation{}, false
	}
	location := moduleLocation{repository: repository}
	if nested == "" {
		return location, true
	}
	prefix, major, ok := module.SplitPathVersion(modulePath)
	if !ok {
		return moduleLocation{}, false
	}
	location.subdir = strings.TrimPrefix(strings.TrimPrefix(prefix, namespace+"/"+root), "/")
	location.major = major
	return location, true
}

// Resolve the module directory from the pinned commit, including Go's /vN
// layout. The same immutable commit determines both metadata and zip contents.
func (h *Handler) moduleDirectory(ctx context.Context, repository, modulePath, source string) (string, []byte, error) {
	location, ok := h.moduleLocation(modulePath)
	if !ok {
		return "", nil, &proxyBuildError{class: "private_unavailable", status: http.StatusForbidden}
	}
	dir := location.subdir
	mod, err := h.cache.ShowFile(ctx, repository, source, path.Join(dir, "go.mod"))
	if location.subdir == "" && location.major == "" {
		if err != nil {
			return "", nil, moduleReadBuildError(err)
		}
		return dir, mod, nil
	}
	if err != nil && !errors.Is(err, gitcache.ErrModuleFileMissing) {
		return "", nil, moduleReadBuildError(err)
	}
	valid := err == nil && modfile.ModulePath(mod) == modulePath
	if location.major != "" {
		alternate := path.Join(dir, strings.TrimPrefix(location.major, "/"))
		other, otherErr := h.cache.ShowFile(ctx, repository, source, path.Join(alternate, "go.mod"))
		if otherErr != nil && !errors.Is(otherErr, gitcache.ErrModuleFileMissing) {
			return "", nil, moduleReadBuildError(otherErr)
		}
		if otherErr == nil {
			if valid || modfile.ModulePath(other) != modulePath {
				return "", nil, modulePathMismatch()
			}
			return alternate, other, nil
		}
	}
	if valid {
		return dir, mod, nil
	}
	if err != nil {
		return "", nil, moduleReadBuildError(err)
	}
	return "", nil, modulePathMismatch()
}

func modulePathMismatch() *proxyBuildError {
	return &proxyBuildError{class: "module_path_mismatch", status: http.StatusForbidden}
}
