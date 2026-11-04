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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"testing"
	"time"

	"golang.org/x/mod/module"

	"github.com/oberthci/oberth/internal/gitcache"
)

const pseudoVersion = "v0.0.0-20260929120000-0123456789ab"

type pseudoGit struct {
	*artifactGit
	blocked    atomic.Bool
	outage     atomic.Bool
	branchOnly atomic.Bool
	changed    atomic.Bool
}

func (g *pseudoGit) ModulePseudoVersionSnapshot(_ context.Context, _, _, _ string, allow bool) (string, time.Time, error) {
	if g.outage.Load() {
		return "", time.Time{}, errors.New("backend-canary")
	}
	if g.blocked.Load() || g.branchOnly.Load() && !allow {
		return "", time.Time{}, gitcache.ErrModulePseudoVersionUnavailable
	}
	source := artifactOID
	if g.changed.Load() {
		source = strings.Repeat("f", 40)
	}
	return source, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), nil
}
func newPseudoHandler(t *testing.T, g GitCache, root string, allow bool) *Handler {
	t.Helper()
	h, err := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": "forge/owner/repository"}).WithPseudoVersionsOnNonDefaultBranches(allow).WithArtifactCache(root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func pseudoRequest(h http.Handler, version, ext string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/"+artifactModule+"/@v/"+version+ext, nil))
	return r
}
func TestPseudoVersionArtifactsAndDiscovery(t *testing.T) {
	g := &pseudoGit{artifactGit: newArtifactGit()}
	h, err := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": "forge/owner/repository"}).WithArtifactCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var firstZip []byte
	for _, ext := range []string{".info", ".mod", ".zip", ".zip"} {
		r := pseudoRequest(h, pseudoVersion, ext)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", ext, r.Code, r.Body)
		}
		if ext == ".info" {
			var info map[string]string
			if err = json.Unmarshal(r.Body.Bytes(), &info); err != nil || info["Version"] != pseudoVersion || info["Time"] != "2026-09-29T12:00:00Z" {
				t.Fatalf("info=%v %v", info, err)
			}
		}
		if ext == ".mod" && r.Body.String() != artifactMod {
			t.Fatal("go.mod changed")
		}
		if ext == ".zip" {
			if firstZip == nil {
				firstZip = bytes.Clone(r.Body.Bytes())
			} else if !bytes.Equal(firstZip, r.Body.Bytes()) {
				t.Fatal("zip changed")
			}
		}
	}
	if g.archives.Load() != 1 {
		t.Fatal("zip regenerated")
	}
	for _, suffix := range []string{"@v/list", "@latest"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/"+artifactModule+"/"+suffix, nil))
		if r.Code != 200 || bytes.Contains(r.Body.Bytes(), []byte(pseudoVersion)) || !bytes.Contains(r.Body.Bytes(), []byte("v1.0.0")) {
			t.Fatalf("discovery changed: %d %s", r.Code, r.Body)
		}
	}
}

func TestPseudoVersionWarmPolicyImmutabilityAndRestart(t *testing.T) {
	root := t.TempDir()
	g := &pseudoGit{artifactGit: newArtifactGit()}
	h := newPseudoHandler(t, g, root, false)
	want := map[string][]byte{}
	etags := map[string]string{}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := pseudoRequest(h, pseudoVersion, ext)
		if r.Code != 200 {
			t.Fatal(r.Body)
		}
		want[ext] = bytes.Clone(r.Body.Bytes())
		etags[ext] = r.Header().Get("ETag")
	}
	g.blocked.Store(true)
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/"+artifactModule+"/@v/"+pseudoVersion+ext, nil)
		req.Header.Set("If-None-Match", etags[ext])
		h.ServeHTTP(r, req)
		if r.Code != 410 || r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("X-Oberth-GoProxy-Error") != "pseudo_version_unavailable" {
			t.Fatalf("unreachable cached %s: %d %s", ext, r.Code, r.Body)
		}
	}
	// A denied result must never poison a later valid request. A new process
	// reads the same immutable files and still rechecks current authority.
	g.blocked.Store(false)
	h = newPseudoHandler(t, g, root, false)
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := pseudoRequest(h, pseudoVersion, ext)
		if r.Code != 200 || !bytes.Equal(want[ext], r.Body.Bytes()) || etags[ext] != r.Header().Get("ETag") || r.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("restart %s: %d %s", ext, r.Code, r.Body)
		}
	}
	g.changed.Store(true)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 410 {
		t.Fatalf("rebound source served: %d %s", r.Code, r.Body)
	}
	g.changed.Store(false)
	g.outage.Store(true)
	if r := pseudoRequest(h, pseudoVersion, ".info"); r.Code != 503 || r.Header().Get("Retry-After") != "1" || strings.Contains(r.Body.String(), "canary") {
		t.Fatalf("backend policy failure: %d %s", r.Code, r.Body)
	}
	g.outage.Store(false)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), want[".zip"]) {
		t.Fatal("source failure changed immutable content")
	}
	if g.archives.Load() != 1 {
		t.Fatal("warm artifacts regenerated")
	}
	// Explicit administrator revocation still takes precedence over any source.
	marker := filepath.Join(root, "revoked", artifactKey("forge/owner/repository", artifactModule, pseudoVersion))
	if err := os.WriteFile(marker, []byte("deny"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 403 {
		t.Fatalf("revocation bypassed: %d", r.Code)
	}
}

func TestPseudoVersionBranchPolicyAndConcurrentFill(t *testing.T) {
	root := t.TempDir()
	g := &pseudoGit{artifactGit: newArtifactGit()}
	g.branchOnly.Store(true)
	h := newPseudoHandler(t, g, root, false)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 410 {
		t.Fatal("default policy widened")
	}
	h = newPseudoHandler(t, g, root, true)
	var wg sync.WaitGroup
	for i := range 18 {
		wg.Go(func() {
			ext := []string{".info", ".mod", ".zip"}[i%3]
			r := pseudoRequest(h, pseudoVersion, ext)
			if r.Code != 200 {
				t.Errorf("parallel %s: %d %s", ext, r.Code, r.Body)
			}
		})
	}
	wg.Wait()
	if g.archives.Load() != 1 {
		t.Fatalf("parallel zip builds=%d", g.archives.Load())
	}
	h = newPseudoHandler(t, g, root, false)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 410 {
		t.Fatal("cached opt-in bypassed later default-only policy")
	}
}

type revokeDuringArchive struct{ *pseudoGit }

func (g *revokeDuringArchive) ArchiveTar(ctx context.Context, input, source string, w io.Writer) error {
	err := g.artifactGit.ArchiveTar(ctx, input, source, w)
	g.blocked.Store(true)
	return err
}
func TestPseudoVersionRechecksPolicyBeforeWritingResponse(t *testing.T) {
	g := &revokeDuringArchive{pseudoGit: &pseudoGit{artifactGit: newArtifactGit()}}
	h := newPseudoHandler(t, g, t.TempDir(), false)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 410 || bytes.HasPrefix(r.Body.Bytes(), []byte("PK")) {
		t.Fatalf("changed policy leaked zip: %d %s", r.Code, r.Body)
	}
	g.blocked.Store(false)
	if r := pseudoRequest(h, pseudoVersion, ".zip"); r.Code != 200 || g.archives.Load() != 1 {
		t.Fatalf("completed immutable zip regenerated: %d %s", r.Code, r.Body)
	}
}

func TestGoResolverPseudoVersionDownloadAndSums(t *testing.T) {
	g := &pseudoGit{artifactGit: newArtifactGit()}
	h := newPseudoHandler(t, g, t.TempDir(), false)
	f := newResolverFixture(t, h, http.StatusNotFound)
	output, err := f.run(t, "mod", "download", "-json", artifactModule+"@"+pseudoVersion)
	var result struct{ Sum, GoModSum string }
	if err != nil || json.Unmarshal([]byte(output), &result) != nil || !strings.HasPrefix(result.Sum, "h1:") || !strings.HasPrefix(result.GoModSum, "h1:") {
		t.Fatalf("Go pseudo download: %v %s", err, output)
	}
	sums := artifactModule + " " + pseudoVersion + " " + result.Sum + "\n" + artifactModule + " " + pseudoVersion + "/go.mod " + result.GoModSum + "\n"
	output, err = f.runWithSum(t, sums, "mod", "download", artifactModule+"@"+pseudoVersion)
	if err != nil {
		t.Fatalf("Go sums: %v %s", err, output)
	}
	if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("successful pseudo lookup fell through: %v %v", f.public.all(), f.direct.all())
	}
}

func TestGoResolverPseudoVersionDenialFallbackIsExplicit(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(strings.ToLower(fmt.Sprint(outage)), func(t *testing.T) {
			g := &pseudoGit{artifactGit: newArtifactGit()}
			g.blocked.Store(!outage)
			g.outage.Store(outage)
			f := newResolverFixture(t, newPseudoHandler(t, g, t.TempDir(), false), http.StatusNotFound)
			_, err := f.run(t, "mod", "download", artifactModule+"@"+pseudoVersion)
			if err == nil {
				t.Fatal("invalid pseudo downloaded")
			}
			if outage {
				if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
					t.Fatal("retryable backend error fell through")
				}
			} else if len(f.public.all()) == 0 {
				t.Fatal("410 did not have documented fallback semantics")
			}
		})
	}
}

func TestRealGitPseudoVersionForcePushAndRestore(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T09:08:07+02:00")
	work := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(work, "init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(artifactMod), 0600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-m", "base")
	git(work, "tag", "v1.0.0")
	base := git(work, "rev-parse", "HEAD")
	git(work, "commit", "--allow-empty", "-m", "untagged")
	source := git(work, "rev-parse", "HEAD")
	cache, err := gitcache.New(gitcache.Config{AllowLocalUpstreams: true, Root: t.TempDir(), Upstream: func(string) (string, error) { return work, nil }})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	// Resolution must use only mirrored objects, without contacting upstream.
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	newHandler := func(allow bool) *Handler {
		h, err := NewHandler(cache, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithPseudoVersionsOnNonDefaultBranches(allow).WithArtifactCache(root)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := newHandler(false)
	version := module.PseudoVersion("v1", "v1.0.0", time.Date(2026, 10, 1, 7, 8, 7, 0, time.UTC), source[:12])
	want := map[string][]byte{}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := pseudoRequest(h, version, ext)
		if r.Code != 200 {
			t.Fatalf("fill %s: %d %s", ext, r.Code, r.Body)
		}
		want[ext] = bytes.Clone(r.Body.Bytes())
	}
	git(repo.Path, "update-ref", "refs/heads/trunk", base)
	for ext := range want {
		if r := pseudoRequest(h, version, ext); r.Code != 410 {
			t.Fatalf("force-push warm %s: %d %s", ext, r.Code, r.Body)
		}
	}
	git(repo.Path, "update-ref", "refs/heads/feature", source)
	if r := pseudoRequest(h, version, ".zip"); r.Code != 410 {
		t.Fatalf("default-only policy expanded: %d %s", r.Code, r.Body)
	}
	if r := pseudoRequest(newHandler(true), version, ".zip"); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), want[".zip"]) {
		t.Fatalf("opt-in altered pinned bytes: %d %s", r.Code, r.Body)
	}
	git(repo.Path, "update-ref", "-d", "refs/heads/feature")
	git(repo.Path, "update-ref", "refs/heads/trunk", source)
	h = newHandler(false)
	for ext, body := range want {
		if r := pseudoRequest(h, version, ext); r.Code != 200 || !bytes.Equal(body, r.Body.Bytes()) {
			t.Fatalf("restored %s changed: %d %s", ext, r.Code, r.Body)
		}
	}
}
