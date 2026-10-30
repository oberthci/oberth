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
	"sync"
	"time"
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
	for _, dir := range []string{"objects", "versions"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return nil, fmt.Errorf("initialize module artifact cache: %w", err)
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
	file, err := os.Open(c.versionPath(artifactKey(repository, modulePath, version))) //nolint:gosec // Digest-only path under the private cache root.
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
	if len(data) > 8192 || json.Unmarshal(data, &entry) != nil || entry.Format != 1 || entry.Repository != repository || entry.Module != modulePath || entry.Version != version || entry.Source == "" || len(entry.Objects) != 3 {
		return nil, errors.New("invalid immutable module manifest")
	}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		object, ok := entry.Objects[ext]
		if !ok || !validArtifactDigest(object.SHA256) || object.Size < 0 || object.Size > 500<<20 {
			return nil, errors.New("invalid immutable module object")
		}
	}
	return &entry, nil
}

func (c *artifactCache) get(ctx context.Context, repository, modulePath, version string, build func(context.Context) (string, map[string][]byte, error)) (*artifactVersion, error) {
	if ctx.Err() != nil {
		return nil, backendBuildError()
	}
	entry, err := c.lookup(repository, modulePath, version)
	if err != nil || entry != nil {
		return entry, err
	}
	key := artifactKey(repository, modulePath, version)
	c.mu.Lock()
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, backendBuildError()
		case <-flight.done:
			return flight.version, flight.err
		}
	}
	flight := &artifactFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if flight.version == nil && flight.err == nil {
			flight.err = errors.New("module artifact generation aborted")
		}
		delete(c.flights, key)
		close(flight.done)
		c.mu.Unlock()
	}()
	select {
	case c.builds <- struct{}{}:
		defer func() { <-c.builds }()
	case <-ctx.Done():
		flight.err = backendBuildError()
		return nil, flight.err
	}
	// Another process may have committed while this request waited for a slot.
	entry, err = c.lookup(repository, modulePath, version)
	if err == nil && entry == nil {
		var source string
		var objects map[string][]byte
		source, objects, err = build(ctx)
		if err == nil {
			entry = &artifactVersion{Format: 1, Repository: repository, Module: modulePath, Version: version, Source: source, Objects: make(map[string]artifactObject)}
			for ext, data := range objects {
				digest := sha256.Sum256(data)
				object := artifactObject{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
				if object.Size > 500<<20 {
					err = errors.New("immutable module object exceeds limit")
					break
				}
				if err = publishArtifact(ctx, c.objectPath(object.SHA256), data); err != nil {
					break
				}
				entry.Objects[ext] = object
			}
			if err == nil {
				var manifest []byte
				manifest, err = json.Marshal(entry)
				if err == nil {
					err = publishArtifact(ctx, c.versionPath(key), manifest)
				}
			}
		}
	}
	flight.version, flight.err = entry, err
	return entry, err
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
	entry, err := h.artifacts.get(ctx, repository, modulePath, version, func(ctx context.Context) (string, map[string][]byte, error) {
		var source string
		var data map[string][]byte
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			attemptCtx, stop := context.WithTimeout(ctx, h.backendBudget)
			source, data, err = h.buildVersion(attemptCtx, repository, modulePath, version)
			stop()
			var failure *proxyBuildError
			if err == nil || !errors.As(err, &failure) || failure.status != http.StatusServiceUnavailable || attempt == 1 {
				break
			}
			select {
			case <-ctx.Done():
				return "", nil, backendBuildError()
			case <-time.After(250 * time.Millisecond):
			}
		}
		return source, data, err
	})
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
	w.Header().Set("Content-Type", map[string]string{".info": "application/json", ".mod": "text/plain; charset=utf-8", ".zip": "application/zip"}[ext])
	w.Header().Set("ETag", `"`+object.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
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
		return "", nil, backendBuildError()
	}
	archive, err := h.buildZip(ctx, repository, modulePath, version, source)
	if err != nil {
		return "", nil, err
	}
	info, err := json.Marshal(map[string]string{"Version": version, "Time": timestamp.UTC().Format(time.RFC3339)})
	if err != nil {
		return "", nil, err
	}
	return source, map[string][]byte{".info": append(info, '\n'), ".mod": mod, ".zip": archive}, nil
}

type proxyBuildError struct {
	class  string
	status int
}

func (e *proxyBuildError) Error() string { return e.class }
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
