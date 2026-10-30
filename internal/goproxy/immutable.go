package goproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

// moduleTagSnapshot pins all representations to one immutable Git object.
// Public tag names are never used again after this resolution.
type moduleTagSnapshot interface {
	ModuleTagSnapshot(context.Context, string, string) (string, time.Time, error)
}

type artifactObject struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type artifactVersion struct {
	Format     int                       `json:"format"`
	Repository string                    `json:"repository"`
	Module     string                    `json:"module"`
	Version    string                    `json:"version"`
	Source     string                    `json:"source"`
	Objects    map[string]artifactObject `json:"objects"`
}

type artifactFlight struct {
	done    chan struct{}
	version *artifactVersion
	err     error
}

type artifactCache struct {
	root    string
	mu      sync.Mutex
	flights map[string]*artifactFlight
	builds  chan struct{}
}

// WithArtifactCache enables persistent, immutable version bundles. Configure
// before serving. Cache roots are administrator-owned, never pipeline mounts.
func (h *Handler) WithArtifactCache(root string) (*Handler, error) {
	if h.modulePrefix == "" {
		return nil, errors.New("invalid module proxy namespace")
	}
	if _, ok := h.cache.(moduleTagSnapshot); !ok {
		return nil, errors.New("module proxy backend cannot pin immutable tags")
	}
	if root == "" {
		return nil, errors.New("module artifact cache root is required")
	}
	for _, dir := range []string{"", "objects", "versions", "revoked"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return nil, fmt.Errorf("initialize module artifact cache: %w", err)
		}
		if err := os.Chmod(filepath.Join(root, dir), 0700); err != nil { //nolint:gosec // Directories require owner execute/search permission.
			return nil, fmt.Errorf("secure module artifact cache: %w", err)
		}
	}
	// Persist newly created directory entries as well as their later contents.
	for _, dir := range []string{root, filepath.Dir(root)} {
		if err := syncArtifactDirectory(dir); err != nil {
			return nil, fmt.Errorf("sync module artifact cache directory: %w", err)
		}
	}
	h.artifacts = &artifactCache{root: root, flights: make(map[string]*artifactFlight), builds: make(chan struct{}, 2)}
	return h, nil
}

func artifactKey(repository, modulePath, version string) string {
	digest := sha256.Sum256([]byte("module-artifact-v1\x00" + repository + "\x00" + modulePath + "\x00" + version))
	return hex.EncodeToString(digest[:])
}

func (c *artifactCache) versionPath(key string) string {
	return filepath.Join(c.root, "versions", key[:2], key+".json")
}
func (c *artifactCache) objectPath(digest string) string {
	return filepath.Join(c.root, "objects", digest[:2], digest)
}

func validArtifactDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == value
}

func (c *artifactCache) lookup(repository, modulePath, version string) (*artifactVersion, error) {
	return c.lookupStage(repository, modulePath, version, "")
}

// Zip publication is separate so metadata never depends on tree extraction.
func (c *artifactCache) lookupStage(repository, modulePath, version, stage string) (*artifactVersion, error) {
	file, err := os.Open(c.versionPath(artifactKey(repository, modulePath, version) + stage)) //nolint:gosec // Digest-only path under the private cache root.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, err
	}
	var entry artifactVersion
	if len(data) > 8192 || json.Unmarshal(data, &entry) != nil || entry.Format != 1 || entry.Repository != repository || entry.Module != modulePath || entry.Version != version || entry.Source == "" {
		return nil, errors.New("invalid immutable module manifest")
	}
	extensions := []string{".info", ".mod"}
	if stage == ".zip" {
		extensions = []string{".zip"}
	} else if len(entry.Objects) == 3 {
		extensions = append(extensions, ".zip")
	} // R1 manifests remain readable.
	if len(entry.Objects) != len(extensions) {
		return nil, errors.New("invalid immutable module manifest objects")
	}
	for _, ext := range extensions {
		object, ok := entry.Objects[ext]
		if !ok || !validArtifactDigest(object.SHA256) || object.Size < 0 || object.Size > 500<<20 {
			return nil, errors.New("invalid immutable module object")
		}
	}
	return &entry, nil
}

// Each waiter owns only its wait. Shared work has a separate, bounded lifetime,
// including build-slot admission; disconnecting the first client cannot abort it.
func (c *artifactCache) get(ctx context.Context, repository, modulePath, version, stage string, budget time.Duration, build func(context.Context) (string, map[string][]byte, error)) (*artifactVersion, error) {
	if ctx.Err() != nil {
		return nil, backendBuildError()
	}
	entry, err := c.lookupStage(repository, modulePath, version, stage)
	if err != nil || entry != nil {
		return entry, err
	}
	key := artifactKey(repository, modulePath, version) + stage
	c.mu.Lock()
	flight := c.flights[key]
	if flight == nil {
		flight = &artifactFlight{done: make(chan struct{})}
		c.flights[key] = flight
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		go func() {
			defer cancel()
			entry, err := c.fill(workCtx, repository, modulePath, version, stage, build)
			c.mu.Lock()
			flight.version, flight.err = entry, err
			delete(c.flights, key)
			close(flight.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, backendBuildError()
	case <-flight.done:
		return flight.version, flight.err
	}
}

func (c *artifactCache) fill(ctx context.Context, repository, modulePath, version, stage string, build func(context.Context) (string, map[string][]byte, error)) (*artifactVersion, error) {
	select {
	case c.builds <- struct{}{}:
		defer func() { <-c.builds }()
	case <-ctx.Done():
		return nil, backendBuildError()
	}
	// Another process may have published while this fill waited for a slot.
	entry, err := c.lookupStage(repository, modulePath, version, stage)
	if err != nil || entry != nil {
		return entry, err
	}
	source, objects, err := build(ctx)
	if err != nil {
		return nil, err
	}
	entry = &artifactVersion{Format: 1, Repository: repository, Module: modulePath, Version: version, Source: source, Objects: make(map[string]artifactObject)}
	for ext, data := range objects {
		digest := sha256.Sum256(data)
		object := artifactObject{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
		if object.Size > 500<<20 {
			return nil, errors.New("immutable module object exceeds limit")
		}
		if err := publishArtifact(ctx, c.objectPath(object.SHA256), data); err != nil {
			return nil, err
		}
		entry.Objects[ext] = object
	}
	manifest, err := json.Marshal(entry)
	if err == nil {
		err = publishArtifact(ctx, c.versionPath(artifactKey(repository, modulePath, version)+stage), manifest)
	}
	return entry, err
}

// A durable deny marker takes precedence over both warm and cold artifacts.
// Stat failures fail closed; only a positively absent marker permits serving.
func (c *artifactCache) revoked(repository, modulePath, version string) error {
	_, err := os.Lstat(filepath.Join(c.root, "revoked", artifactKey(repository, modulePath, version))) //nolint:gosec // Coordinate strings enter paths only through a SHA-256 hex key.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return &proxyBuildError{class: "private_unavailable", status: http.StatusForbidden}
}

// Publish content and then its manifest using no-clobber links. Readers cannot
// see partial bytes, crashes leave at most unreferenced objects, and another
// process cannot replace an already committed version with a moved tag.
func publishArtifact(ctx context.Context, target string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if err := syncArtifactDirectory(filepath.Dir(filepath.Dir(target))); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".artifact-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		want := sha256.Sum256(data)
		existing, err := os.Open(target) //nolint:gosec // Only generated digest/manifest paths reach this function.
		if err != nil {
			return err
		}
		defer func() { _ = existing.Close() }()
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(proxyContextReader{ctx, existing}, int64(len(data))+1))
		if err != nil {
			return err
		}
		if n != int64(len(data)) || hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(want[:]) {
			return errors.New("immutable module cache conflict")
		}
	}
	return syncArtifactDirectory(filepath.Dir(target))
}

func syncArtifactDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // Administrator root or generated digest directory.
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, repository, modulePath, version, ext string) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*h.backendBudget+time.Second)
	defer cancel()
	if err := h.artifacts.revoked(repository, modulePath, version); err != nil {
		writeBuildError(w, err)
		return
	}
	budget := 2*h.backendBudget + time.Second
	entry, err := h.artifacts.get(ctx, repository, modulePath, version, "", budget, func(work context.Context) (string, map[string][]byte, error) {
		return h.buildWithRetry(work, func(attempt context.Context) (string, map[string][]byte, error) {
			return h.buildVersion(attempt, repository, modulePath, version)
		})
	})
	if err == nil && ext == ".zip" {
		if _, exists := entry.Objects[".zip"]; !exists {
			source := entry.Source
			var zipEntry *artifactVersion
			zipEntry, err = h.artifacts.get(ctx, repository, modulePath, version, ".zip", budget, func(work context.Context) (string, map[string][]byte, error) {
				return h.buildWithRetry(work, func(attempt context.Context) (string, map[string][]byte, error) {
					data, err := h.buildZip(attempt, repository, modulePath, version, source)
					return source, map[string][]byte{".zip": data}, err
				})
			})
			if err == nil {
				if zipEntry.Source != source {
					err = errors.New("immutable module zip source conflict")
				} else {
					entry = zipEntry
				}
			}
		}
	}
	if err != nil {
		writeBuildError(w, err)
		return
	}
	object := entry.Objects[ext]
	file, err := os.Open(h.artifacts.objectPath(object.SHA256)) //nolint:gosec // Manifest digests were validated on read or generated locally.
	if err != nil {
		writeBuildError(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	// Verify before emitting headers, then stream from the same descriptor. The
	// second pass usually reads page cache, and never serves unchecked bytes.
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(proxyContextReader{ctx, file}, object.Size+1))
	if err != nil || n != object.Size || hex.EncodeToString(hash.Sum(nil)) != object.SHA256 {
		if ctx.Err() != nil {
			backendUnavailable(w)
			return
		}
		writeBuildError(w, errors.New("immutable module object integrity failure"))
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeBuildError(w, err)
		return
	}
	if err := h.artifacts.revoked(repository, modulePath, version); err != nil {
		writeBuildError(w, err)
		return
	}
	w.Header().Set("Content-Type", map[string]string{".info": "application/json", ".mod": "text/plain; charset=utf-8", ".zip": "application/zip"}[ext])
	w.Header().Set("ETag", `"`+object.SHA256+`"`)
	if strings.HasSuffix(r.URL.Path, "/@latest") {
		w.Header().Set("Cache-Control", "private, no-cache")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, version+ext, time.Time{}, file)
}

func (h *Handler) buildVersion(ctx context.Context, repository, modulePath, version string) (string, map[string][]byte, error) {
	versions, err := h.versions(ctx, repository, modulePath)
	if err != nil {
		return "", nil, backendBuildError()
	}
	found := false
	for _, known := range versions {
		found = found || known == version
	}
	if !found {
		return "", nil, &proxyBuildError{class: "private_unavailable", status: http.StatusForbidden}
	}
	source, timestamp, err := h.cache.(moduleTagSnapshot).ModuleTagSnapshot(ctx, repository, version)
	if err != nil {
		return "", nil, backendBuildError()
	}
	mod, err := h.cache.ShowFile(ctx, repository, source, "go.mod")
	if err != nil {
		return "", nil, moduleReadBuildError(err)
	}
	info, err := json.Marshal(map[string]string{"Version": version, "Time": timestamp.UTC().Format(time.RFC3339)})
	if err != nil {
		return "", nil, err
	}
	return source, map[string][]byte{".info": append(info, '\n'), ".mod": mod}, nil
}

func (h *Handler) buildWithRetry(ctx context.Context, build func(context.Context) (string, map[string][]byte, error)) (string, map[string][]byte, error) {
	for attempt := 0; ; attempt++ {
		attemptCtx, stop := context.WithTimeout(ctx, h.backendBudget)
		source, data, err := build(attemptCtx)
		stop()
		var failure *proxyBuildError
		if err == nil || !errors.As(err, &failure) || failure.status != http.StatusServiceUnavailable || attempt == 1 {
			return source, data, err
		}
		select {
		case <-ctx.Done():
			return "", nil, backendBuildError()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type proxyBuildError struct {
	class  string
	status int
}

func (e *proxyBuildError) Error() string { return e.class }
func moduleReadBuildError(err error) *proxyBuildError {
	if errors.Is(err, gitcache.ErrModuleSourceMissing) {
		return &proxyBuildError{class: "pinned_source_missing", status: http.StatusInternalServerError}
	}
	return backendBuildError()
}

// Discovery stays live, but a denied version cannot be advertised as usable.
// Unknown marker state fails closed instead of silently skipping a policy error.
func (h *Handler) visibleVersions(ctx context.Context, repository, modulePath string, versions []string) ([]string, error) {
	if h.artifacts == nil {
		return versions, nil
	}
	visible := versions[:0]
	for _, version := range versions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := h.artifacts.revoked(repository, modulePath, version); err != nil {
			var denial *proxyBuildError
			if errors.As(err, &denial) && denial.status == http.StatusForbidden {
				continue
			}
			return nil, err
		}
		visible = append(visible, version)
	}
	return visible, nil
}

func backendBuildError() *proxyBuildError {
	return &proxyBuildError{class: "private_backend_unavailable", status: http.StatusServiceUnavailable}
}
func backendUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	proxyError(w, "private_backend_unavailable", "private repository unavailable; retry the request", http.StatusServiceUnavailable)
}
func writeBuildError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		backendUnavailable(w)
		return
	}
	var failure *proxyBuildError
	if errors.As(err, &failure) {
		switch failure.status {
		case http.StatusServiceUnavailable:
			backendUnavailable(w)
		case http.StatusForbidden:
			privateUnavailable(w)
		default:
			proxyError(w, failure.class, "module generation failed", failure.status)
		}
		return
	}
	proxyError(w, "artifact_cache_error", "module artifact cache unavailable", http.StatusInternalServerError)
}

type proxyContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r proxyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type proxyContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w proxyContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

func (h *Handler) versionsWithRetry(ctx context.Context, repository, modulePath string) ([]string, error) {
	var versions []string
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		attemptCtx, stop := context.WithTimeout(ctx, h.backendBudget)
		versions, err = h.versions(attemptCtx, repository, modulePath)
		stop()
		if err == nil {
			return versions, nil
		}
		if attempt == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, err
}
