// Package goproxy serves the GOPROXY protocol read-only from Oberth's own
// git cache, enabling credential-free `go mod tidy -diff` for configured
// private modules in pipeline pods (#662).
//
// The handler serves only repositories Oberth already mirrors. It runs on a
// ClusterIP Service in the pipeline namespace, reachable only from Argo pods.
// It does not serve on the public/proxied ingress and requires no auth token
// because it exposes only source Oberth already hosts for the same trust tier.
package goproxy

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"
)

// GitCache is the minimal interface the handler needs from the git cache.
type GitCache interface {
	// ListTags returns all tag names (without refs/tags/ prefix) for the repo.
	ListTags(ctx context.Context, input string) ([]string, error)
	// TagTime returns the tagger date for an annotated tag, or the commit date
	// for a lightweight tag.
	TagTime(ctx context.Context, input, tag string) (time.Time, error)
	// ShowFile returns the contents of a single file at the given ref.
	ShowFile(ctx context.Context, input, ref, filePath string) ([]byte, error)
	// ArchiveTar writes a tar archive of the tree at the given ref.
	ArchiveTar(ctx context.Context, input, ref string, w io.Writer) error
}

// Handler serves the GOPROXY protocol (/@v/list, .info, .mod, .zip) read-only.
type Handler struct {
	failureLogs failureLogLimiter

	logger       *log.Logger
	cache        GitCache
	modulePrefix string
	// repoForModule maps a module suffix (e.g. "wire") to the git cache input
	// string (e.g. "forge/example/sample-wire").
	repoForModule map[string]string
	artifacts     *artifactCache
	backendBudget time.Duration
}

// NewHandler builds a GOPROXY handler that resolves modules against the
// given cache. repoMap maps module suffixes to git cache input strings.
func NewHandler(cache GitCache, namespace string, repoMap map[string]string) *Handler {
	// A malformed/absent namespace must never turn private failures into
	// permission to try public proxies. Invalid construction denies all paths.
	encoded, err := module.EscapePath(namespace)
	if err != nil || ValidateModulePrefix(namespace) != nil {
		return &Handler{cache: cache}
	}
	copied := make(map[string]string, len(repoMap))
	for suffix, input := range repoMap {
		copied[suffix] = input
	}
	return &Handler{cache: cache, modulePrefix: encoded, repoForModule: copied, backendBudget: 15 * time.Second}
}

// ServeHTTP handles GOPROXY protocol requests.
// AI-INVARIANT: classify the private namespace before parsing the protocol.
// No private failure may return 404/410: Go interprets either as permission
// to try the next proxy. The injected comma after this mirror is mandatory.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	observed := &observedResponse{ResponseWriter: w}
	h.serveHTTP(observed, r)
	if h.logger == nil {
		return
	}
	class := observed.Header().Get("X-Oberth-GoProxy-Error")
	if observed.writeErr != nil {
		class = "response_write_failed"
	}
	if class != "" {
		suppressed, allowed := h.failureLogs.allow(class, time.Now())
		if !allowed {
			return
		}
		// Never log queries, credentials or raw cache/backend errors. Bound and
		// quote the path so an invalid request cannot forge or flood log lines.
		urlPath := r.URL.EscapedPath()
		if len(urlPath) > 256 {
			urlPath = urlPath[:256]
		}
		method := r.Method
		if len(method) > 32 {
			method = method[:32]
		}
		h.logger.Printf("goproxy request failed: class=%s status=%d method=%q path=%q suppressed=%d", class, observed.status, method, urlPath, suppressed)
	}
}

// WithLogger enables rate-limited failure diagnostics. Configure it before serving requests.
func (h *Handler) WithLogger(logger *log.Logger) *Handler {
	h.logger = logger
	return h
}

func (h *Handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		proxyError(w, "method_not_allowed", "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.modulePrefix == "" {
		privateUnavailable(w)
		return
	}
	urlPath := strings.TrimLeft(r.URL.Path, "/")
	if urlPath != h.modulePrefix && !strings.HasPrefix(urlPath, h.modulePrefix+"/") {
		http.NotFound(w, r) // Public modules intentionally use the next proxy.
		return
	}
	if r.URL.Path != "/"+urlPath || path.Clean(r.URL.Path) != r.URL.Path || r.URL.RawQuery != "" {
		privateUnavailable(w)
		return
	}

	var encodedModule, versionRequest string
	if strings.HasSuffix(urlPath, "/@latest") {
		encodedModule = strings.TrimSuffix(urlPath, "/@latest")
		versionRequest = "@latest"
	} else if at := strings.Index(urlPath, "/@v/"); at >= 0 {
		encodedModule, versionRequest = urlPath[:at], urlPath[at+4:]
	} else {
		privateUnavailable(w)
		return
	}
	modulePath, err := module.UnescapePath(encodedModule)
	if err != nil || module.CheckPath(modulePath) != nil {
		privateUnavailable(w)
		return
	}
	namespace, _ := module.UnescapePath(h.modulePrefix)
	suffix := strings.TrimPrefix(modulePath, namespace+"/")
	repoInput, ok := h.repoForModule[suffix]
	if !ok || suffix == "" || strings.Contains(suffix, "/") {
		privateUnavailable(w)
		return
	}

	var version, extension string
	if versionRequest != "list" && versionRequest != "@latest" {
		extension = path.Ext(versionRequest)
		if extension != ".info" && extension != ".mod" && extension != ".zip" {
			privateUnavailable(w)
			return
		}
		version, err = module.UnescapeVersion(strings.TrimSuffix(versionRequest, extension))
		if err != nil || !supportedVersion(modulePath, version) {
			privateUnavailable(w)
			return
		}
	}

	if h.artifacts != nil && extension != "" {
		h.serveImmutable(w, r, repoInput, modulePath, version, extension)
		return
	}
	// One shared request budget also covers repository-lock and cold-fill waits.
	ctx, cancel := context.WithTimeout(r.Context(), 2*h.backendBudget+time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	// Confirm exact tag membership before reading any git object. This also
	// prevents a semver-shaped branch or missing tag from being served.
	versions, err := h.versionsWithRetry(r.Context(), repoInput, modulePath)
	if err != nil {
		backendUnavailable(w)
		return
	}
	if versionRequest == "list" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, v := range versions {
			_, _ = fmt.Fprintln(w, v) // #nosec G705 -- canonical module versions only; text/plain protocol.
		}
		return
	}
	if versionRequest == "@latest" {
		// Prefer the highest release, falling back to the highest prerelease.
		for _, v := range versions {
			if version == "" || semver.Prerelease(v) == "" || semver.Prerelease(version) != "" {
				version = v
			}
		}
		extension = ".info"
	}
	found := false
	for _, v := range versions {
		found = found || v == version
	}
	if !found {
		privateUnavailable(w)
		return
	}
	switch extension {
	case ".info":
		h.serveInfo(r.Context(), w, repoInput, version)
	case ".mod":
		h.serveMod(r.Context(), w, repoInput, modulePath, version)
	case ".zip":
		h.serveZip(r.Context(), w, repoInput, modulePath, version)
	}
}

func privateUnavailable(w http.ResponseWriter) {
	proxyError(w, "private_unavailable", "private module or version unavailable", http.StatusForbidden)
}

func supportedVersion(modulePath, version string) bool {
	return module.CanonicalVersion(version) == version && !module.IsPseudoVersion(version) && module.Check(modulePath, version) == nil
}

func (h *Handler) versions(ctx context.Context, input, modulePath string) ([]string, error) {
	tags, err := h.cache.ListTags(ctx, input)
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, tag := range tags {
		if supportedVersion(modulePath, tag) {
			versions = append(versions, tag)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return semver.Compare(versions[i], versions[j]) < 0 })
	return versions, nil
}

func (h *Handler) serveInfo(ctx context.Context, w http.ResponseWriter, input, version string) {
	t, err := h.cache.TagTime(ctx, input, version)
	if err != nil {
		backendUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Version": version,
		"Time":    t.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) serveMod(ctx context.Context, w http.ResponseWriter, input, modulePath, version string) {
	content, err := h.cache.ShowFile(ctx, input, version, "go.mod")
	if err != nil {
		backendUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// The content is go.mod read from the git cache (trusted source, not user input).
	_, _ = w.Write(content) // #nosec G705
}

func (h *Handler) serveZip(ctx context.Context, w http.ResponseWriter, input, modulePath, version string) {
	content, err := h.buildZip(ctx, input, modulePath, version, version)
	if err != nil {
		writeBuildError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	_, _ = w.Write(content)
}

func (h *Handler) buildZip(ctx context.Context, input, modulePath, version, ref string) ([]byte, error) {
	// Read the tar archive from git.
	var tarBuf bytes.Buffer
	if err := h.cache.ArchiveTar(ctx, input, ref, &tarBuf); err != nil {
		return nil, &proxyBuildError{class: "private_backend_unavailable", status: http.StatusServiceUnavailable}
	}

	// Extract the tar to a temporary directory so we can build a canonical
	// module zip with golang.org/x/mod/zip.CreateFromDir. This ensures the
	// served zip is byte-identical to what `go mod download` produces:
	// correct entry ordering, zip metadata, and exclusion of nested modules,
	// vendor/ trees, symlinks, and VCS metadata — all handled by the
	// canonical library rather than hand-rolled filters (#726).
	dir, err := os.MkdirTemp("", "goproxy-zip-*")
	if err != nil {
		return nil, &proxyBuildError{class: "internal_error", status: http.StatusInternalServerError}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tr := tar.NewReader(&tarBuf)
	for {
		if ctx.Err() != nil {
			return nil, backendBuildError()
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &proxyBuildError{class: "archive_error", status: http.StatusInternalServerError}
		}
		// Only extract regular files; symlinks and directories are excluded
		// from module zips by spec and CreateFromDir handles this, but we
		// never write non-regular entries to the temp dir in the first place.
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(header.Name)
		// Defense in depth: reject path traversal (git archive is trusted).
		if strings.HasPrefix(name, "..") || strings.Contains(name, "/../") {
			continue
		}
		// Exclude vendor/ trees: Go module zips never include vendor/
		// content. CreateFromDir does not handle this (it is a VCS-layer
		// concern), so we filter during extraction.
		if isVendorPath(name) {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o750); mkErr != nil {
			return nil, &proxyBuildError{class: "internal_error", status: http.StatusInternalServerError}
		}
		f, createErr := os.Create(target) // #nosec G304 -- path from git archive (trusted), traversal checked above
		if createErr != nil {
			return nil, &proxyBuildError{class: "internal_error", status: http.StatusInternalServerError}
		}
		// #nosec G110 -- tar stream from git archive; size bounded by tree.
		if _, cpErr := io.Copy(f, proxyContextReader{ctx, tr}); cpErr != nil {
			_ = f.Close()
			if ctx.Err() != nil {
				return nil, backendBuildError()
			}
			return nil, &proxyBuildError{class: "internal_error", status: http.StatusInternalServerError}
		}
		if err := f.Close(); err != nil {
			return nil, &proxyBuildError{class: "internal_error", status: http.StatusInternalServerError}
		}
	}

	// Build the canonical module zip. CreateFromDir applies all Go module zip
	// rules: nested module exclusion (subdirs with their own go.mod), vendor/
	// tree exclusion, symlink exclusion, deterministic entry ordering, and
	// canonical zip metadata (timestamps, permissions, compression).
	var zipBuf bytes.Buffer
	mv := module.Version{Path: modulePath, Version: version}
	if err := modzip.CreateFromDir(proxyContextWriter{ctx, &zipBuf}, mv, dir); err != nil {
		if ctx.Err() != nil {
			return nil, &proxyBuildError{class: "private_backend_unavailable", status: http.StatusServiceUnavailable}
		}
		return nil, &proxyBuildError{class: "zip_creation_error", status: http.StatusInternalServerError}
	}

	return zipBuf.Bytes(), nil
}

// isVendorPath reports whether name is inside a vendor/ directory.
// Go module zips never include vendor/ content; CreateFromDir does not
// enforce this (it is a VCS-layer concern in the Go toolchain), so the
// caller must filter vendor paths before extraction.
func isVendorPath(name string) bool {
	return name == "vendor" || strings.HasPrefix(name, "vendor/")
}

// Public 404 misses deliberately have no error class: they are normal resolver
// fallthrough, potentially hundreds of requests in a cold dependency graph.
func proxyError(w http.ResponseWriter, class, message string, status int) {
	w.Header().Set("X-Oberth-GoProxy-Error", class)
	http.Error(w, "oberth: Go module proxy: "+message, status)
}

type observedResponse struct {
	http.ResponseWriter
	status   int
	writeErr error
}

func (w *observedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if w.writeErr == nil {
		w.writeErr = err
	}
	return n, err
}
