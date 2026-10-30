package goproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/dirhash"
)

const artifactModule = "go.example.test/issue764-fixture"
const artifactOID = "0123456789abcdef0123456789abcdef01234567"
const artifactMod = "module " + artifactModule + "\n\ngo 1.22\n"

type artifactGit struct {
	*mockCache
	calls        atomic.Int64
	archives     atomic.Int64
	down         atomic.Bool
	slow         atomic.Bool
	firstFailure bool
}

func newArtifactGit() *artifactGit {
	return &artifactGit{mockCache: &mockCache{tags: []string{"v1.0.0"}, files: map[string][]byte{"v1.0.0:go.mod": []byte(artifactMod)}}}
}
func (g *artifactGit) ListTags(ctx context.Context, input string) ([]string, error) {
	count := g.calls.Add(1)
	if g.slow.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if g.down.Load() || (g.firstFailure && count == 1) {
		return nil, errors.New("private-backend-canary")
	}
	return g.mockCache.ListTags(ctx, input)
}
func (g *artifactGit) ModuleTagSnapshot(context.Context, string, string) (string, time.Time, error) {
	g.calls.Add(1)
	return artifactOID, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), nil
}
func (g *artifactGit) ShowFile(ctx context.Context, input, ref, file string) ([]byte, error) {
	g.calls.Add(1)
	if ref != artifactOID {
		return nil, errors.New("mutable tag used after snapshot")
	}
	return g.mockCache.ShowFile(ctx, input, "v1.0.0", file)
}
func (g *artifactGit) ArchiveTar(ctx context.Context, input, ref string, w io.Writer) error {
	g.calls.Add(1)
	g.archives.Add(1)
	if ref != artifactOID {
		return errors.New("mutable tag used after snapshot")
	}
	return g.mockCache.ArchiveTar(ctx, input, "v1.0.0", w)
}
func artifactHandler(t *testing.T, g *artifactGit, root string) *Handler {
	t.Helper()
	h, err := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": "forge/owner/repository"}).WithArtifactCache(root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func artifactRequest(h http.Handler, ext string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/"+artifactModule+"/@v/v1.0.0"+ext, nil))
	return r
}

func TestImmutableArtifactsSurviveRestartAndAvoidEveryGitRead(t *testing.T) {
	root := t.TempDir()
	g := newArtifactGit()
	h := artifactHandler(t, g, root)
	want := make(map[string][]byte)
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := artifactRequest(h, ext)
		if r.Code != 200 {
			t.Fatalf("fill %s: %d %s", ext, r.Code, r.Body)
		}
		want[ext] = bytes.Clone(r.Body.Bytes())
	}
	if g.archives.Load() != 1 || g.calls.Load() != 4 {
		t.Fatalf("version regenerated: reads=%d archives=%d", g.calls.Load(), g.archives.Load())
	}
	g.down.Store(true)
	// A changed tag or backend contents cannot alter a published version.
	g.tags = nil
	g.files = map[string][]byte{}
	h = artifactHandler(t, g, root)
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := artifactRequest(h, ext)
		if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), want[ext]) {
			t.Fatalf("warm %s consulted failed backend or changed: %d %s", ext, r.Code, r.Body)
		}
		if r.Header().Get("ETag") == "" {
			t.Fatal("missing immutable digest")
		}
	}
	if g.calls.Load() != 4 {
		t.Fatal("cache hit performed Git work", g.calls.Load())
	}
}

func TestImmutableArtifactsConcurrentMissProducesOneCompleteVersion(t *testing.T) {
	g := newArtifactGit()
	h := artifactHandler(t, g, t.TempDir())
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := range 24 {
		workers.Go(func() {
			<-start
			ext := []string{".info", ".mod", ".zip"}[i%3]
			r := artifactRequest(h, ext)
			if r.Code != 200 {
				t.Errorf("parallel %s: %d %s", ext, r.Code, r.Body)
			}
		})
	}
	close(start)
	workers.Wait()
	if g.archives.Load() != 1 || g.calls.Load() != 4 || len(h.artifacts.flights) != 0 || len(h.artifacts.builds) != 0 {
		t.Fatalf("duplicate generation or retained admission: calls=%d archives=%d", g.calls.Load(), g.archives.Load())
	}
}

func TestImmutableArtifactTimeoutIsRetryableAndNeverFallsThrough(t *testing.T) {
	g := newArtifactGit()
	g.slow.Store(true)
	h := artifactHandler(t, g, t.TempDir())
	h.backendBudget = 10 * time.Millisecond
	r := artifactRequest(h, ".zip")
	if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" || strings.Contains(r.Body.String(), "private-backend-canary") || g.calls.Load() != 2 {
		t.Fatalf("slow Git did not get two bounded attempts then retryable refusal: %d %s calls=%d", r.Code, r.Body, g.calls.Load())
	}
	if len(h.artifacts.flights) != 0 || len(h.artifacts.builds) != 0 {
		t.Fatal("timed-out build leaked admission")
	}
	g.slow.Store(false)
	if r := artifactRequest(h, ".zip"); r.Code != 200 {
		t.Fatalf("negative result was cached: %d %s", r.Code, r.Body)
	}
}

func TestImmutableArtifactCorruptionFailsClosedWithoutRegenerating(t *testing.T) {
	g := newArtifactGit()
	h := artifactHandler(t, g, t.TempDir())
	if r := artifactRequest(h, ".zip"); r.Code != 200 {
		t.Fatal(r.Body)
	}
	entry, err := h.artifacts.lookup("forge/owner/repository", artifactModule, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.artifacts.objectPath(entry.Objects[".zip"].SHA256), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	g.down.Store(true)
	if r := artifactRequest(h, ".zip"); r.Code != http.StatusInternalServerError || bytes.Contains(r.Body.Bytes(), []byte("corrupt")) {
		t.Fatalf("corrupt object served: %d %s", r.Code, r.Body)
	}
	if g.calls.Load() != 4 {
		t.Fatal("corruption regenerated an immutable version")
	}
}

func artifactGoSums(t *testing.T) (string, string, string) {
	t.Helper()
	prefix := artifactModule + "@v1.0.0/"
	files := map[string]string{prefix + "go.mod": artifactMod, prefix + "wire.go": "package wire\n"}
	zipSum, err := dirhash.Hash1([]string{prefix + "go.mod", prefix + "wire.go"}, func(name string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(files[name])), nil })
	if err != nil {
		t.Fatal(err)
	}
	modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(artifactMod)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s v1.0.0 %s\n%s v1.0.0/go.mod %s\n", artifactModule, zipSum, artifactModule, modSum), zipSum, modSum
}

func TestGoClientImmutableCacheKeepsGoSumAndRecoversTransientGit(t *testing.T) {
	g := newArtifactGit()
	g.firstFailure = true
	root := t.TempDir()
	h := artifactHandler(t, g, root)
	f := newResolverFixture(t, h, http.StatusNotFound)
	sums, zipSum, modSum := artifactGoSums(t)
	output, err := f.runWithSum(t, sums, "mod", "download", "-json", artifactModule+"@v1.0.0")
	var download struct{ Sum, GoModSum string }
	if err != nil || json.Unmarshal([]byte(output), &download) != nil || download.Sum != zipSum || download.GoModSum != modSum {
		t.Fatalf("real Go did not recover or preserve h1: %v %s", err, output)
	}
	reads := g.calls.Load()
	g.down.Store(true)
	output, err = f.runWithSum(t, sums, "mod", "download", "-json", artifactModule+"@v1.0.0")
	if err != nil || g.calls.Load() != reads {
		t.Fatalf("fresh Go cache depended on unavailable Git: %v %s", err, output)
	}
	badSum := strings.ReplaceAll(sums, zipSum, "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	output, err = f.runWithSum(t, badSum, "mod", "download", artifactModule+"@v1.0.0")
	if err == nil || !strings.Contains(output, "checksum mismatch") {
		t.Fatalf("Go accepted changed go.sum: %v %s", err, output)
	}
	if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("private coordinates escaped: public=%v direct=%v", f.public.all(), f.direct.all())
	}
}

func TestGoClient503DoesNotRetryOrLeakPrivateCoordinates(t *testing.T) {
	g := newArtifactGit()
	g.slow.Store(true)
	h := artifactHandler(t, g, t.TempDir())
	h.backendBudget = 10 * time.Millisecond
	f := newResolverFixture(t, h, http.StatusNotFound)
	output, err := f.run(t, "mod", "download", artifactModule+"@v1.0.0")
	if err == nil || !strings.Contains(output, "503") || len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("exhausted private request did not stay retryable/contained: %v %s", err, output)
	}
}

func TestImmutableCacheDoesNotBypassRepositoryMapping(t *testing.T) {
	root := t.TempDir()
	g := newArtifactGit()
	h := artifactHandler(t, g, root)
	if r := artifactRequest(h, ".zip"); r.Code != http.StatusOK {
		t.Fatal(r.Body)
	}
	g.down.Store(true)
	remapped, err := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": "different/owner/repository"}).WithArtifactCache(root)
	if err != nil {
		t.Fatal(err)
	}
	if r := artifactRequest(remapped, ".zip"); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("a different mapped repository borrowed the old source: %d", r.Code)
	}
	assertRoutingStatus(t, h, "/go.example.test/unknown/@v/v1.0.0.zip", http.StatusForbidden)
	assertRoutingStatus(t, h, "/public.example.test/module/@v/v1.0.0.zip", http.StatusNotFound)
}

func TestImmutableCacheFullBuildGateDoesNotBlockWarmReads(t *testing.T) {
	g := newArtifactGit()
	h := artifactHandler(t, g, t.TempDir())
	if r := artifactRequest(h, ".zip"); r.Code != http.StatusOK {
		t.Fatal(r.Body)
	}
	// Occupy both cold-build slots; cache hits must not join their queue.
	h.artifacts.builds <- struct{}{}
	h.artifacts.builds <- struct{}{}
	defer func() { <-h.artifacts.builds; <-h.artifacts.builds }()
	g.down.Store(true)
	if r := artifactRequest(h, ".zip"); r.Code != http.StatusOK || g.calls.Load() != 4 {
		t.Fatalf("warm hit queued behind cold builds: status=%d calls=%d", r.Code, g.calls.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	_, err := h.artifacts.get(ctx, "other/repository", artifactModule, "v1.0.0", func(context.Context) (string, map[string][]byte, error) {
		called = true
		return "", nil, nil
	})
	var failure *proxyBuildError
	if called || !errors.As(err, &failure) || failure.status != http.StatusServiceUnavailable || len(h.artifacts.flights) != 0 {
		t.Fatalf("canceled queued miss generated work or leaked admission: %v", err)
	}
}

func TestImmutablePublicationCannotReplaceCommittedBytes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "objects", "prefix", "committed")
	if err := publishArtifact(t.Context(), target, []byte("first bytes")); err != nil {
		t.Fatal(err)
	}
	if err := publishArtifact(t.Context(), target, []byte("different bytes")); err == nil {
		t.Fatal("a second publisher changed an immutable object")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "first bytes" {
		t.Fatalf("committed content was replaced: %q %v", data, err)
	}
}
