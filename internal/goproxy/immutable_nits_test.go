package goproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func revokeArtifact(t *testing.T, root, repository, version string) (string, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "python3", "../../hack/goproxy-revoke.py", "--cache-root", root, "--repository", repository, "--module", artifactModule, "--version", version)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("revoke: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}
func proxyDiscovery(h http.Handler, suffix string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("GET", "/"+artifactModule+suffix, nil))
	return response
}
func TestRevokeUtilityWarnsOnUnknownCoordinatesButAllowsPreemptiveDenial(t *testing.T) {
	root := t.TempDir()
	h := artifactHandler(t, newArtifactGit(), root)
	if response := artifactRequest(h, ".mod"); response.Code != 200 {
		t.Fatal(response.Body)
	}
	_, warn := revokeArtifact(t, root, "forge/owner/repository", "v1.0.0")
	if warn != "" {
		t.Fatal("matching cached entry warned", warn)
	}
	for range 2 {
		out, warn := revokeArtifact(t, root, "forge/owner/misspelled", "v1.0.0")
		if !strings.Contains(warn, "WARNING: no cached metadata entry") || !strings.Contains(warn, "pre-emptive") || !strings.Contains(out, "Revoked") {
			t.Fatalf("unknown coordinate silently succeeded: %q %q", out, warn)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "revoked", artifactKey("forge/owner/misspelled", artifactModule, "v1.0.0"))); err != nil {
		t.Fatal("preemptive marker not installed", err)
	}
}
func TestImmutableDiscoverySkipsRevokedVersionsAndPinsLatestInfo(t *testing.T) {
	root := t.TempDir()
	g := newArtifactGit()
	g.tags = []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0-rc.1"}
	h := artifactHandler(t, g, root)
	revokeArtifact(t, root, "forge/owner/repository", "v1.2.0")
	list := proxyDiscovery(h, "/@v/list")
	if list.Code != 200 || list.Body.String() != "v1.0.0\nv1.1.0\nv1.3.0-rc.1\n" {
		t.Fatalf("revoked version advertised: %d %s", list.Code, list.Body)
	}
	latest := proxyDiscovery(h, "/@latest")
	var info struct{ Version string }
	if err := json.Unmarshal(latest.Body.Bytes(), &info); err != nil || latest.Code != 200 || info.Version != "v1.1.0" {
		t.Fatalf("latest chose revoked version: %d %s %v", latest.Code, latest.Body, err)
	}
	explicit := proxyDiscovery(h, "/@v/v1.1.0.info")
	if explicit.Code != 200 || !bytes.Equal(explicit.Body.Bytes(), latest.Body.Bytes()) {
		t.Fatal("latest bypassed pinned info")
	}
	revokeArtifact(t, root, "forge/owner/repository", "v1.1.0")
	revokeArtifact(t, root, "forge/owner/repository", "v1.0.0")
	latest = proxyDiscovery(h, "/@latest")
	if err := json.Unmarshal(latest.Body.Bytes(), &info); err != nil || latest.Code != 200 || info.Version != "v1.3.0-rc.1" {
		t.Fatalf("unrevoked prerelease fallback failed: %d %s", latest.Code, latest.Body)
	}
	revokeArtifact(t, root, "forge/owner/repository", "v1.3.0-rc.1")
	if r := proxyDiscovery(h, "/@v/list"); r.Code != 200 || r.Body.Len() != 0 {
		t.Fatal("all-revoked list not empty", r.Code, r.Body)
	}
	if r := proxyDiscovery(h, "/@latest"); r.Code != 403 {
		t.Fatal("all-revoked latest not denied", r.Code, r.Body)
	}
}
func TestImmutableDiscoveryFailsClosedOnUnreadableRevocationState(t *testing.T) {
	root := t.TempDir()
	h := artifactHandler(t, newArtifactGit(), root)
	if err := os.Rename(filepath.Join(root, "revoked"), filepath.Join(root, "revoked.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "revoked"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/@v/list", "/@latest"} {
		if r := proxyDiscovery(h, suffix); r.Code != 500 {
			t.Fatalf("unknown revocation state advertised source: %s %d %s", suffix, r.Code, r.Body)
		}
	}
}

type prunedArtifactGit struct {
	*artifactGit
	failedArchives atomic.Int64
}

func (g *prunedArtifactGit) ArchiveTar(context.Context, string, string, io.Writer) error {
	g.failedArchives.Add(1)
	return fmt.Errorf("archive: %w", gitcache.ErrModuleSourceMissing)
}
func TestPrunedSourceIsPermanentAndNotInternallyRetried(t *testing.T) {
	g := &prunedArtifactGit{artifactGit: newArtifactGit()}
	h, err := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": "forge/owner/repository"}).WithArtifactCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r := artifactRequest(h, ".mod"); r.Code != 200 {
		t.Fatal(r.Body)
	}
	for attempt := int64(1); attempt <= 2; attempt++ {
		r := artifactRequest(h, ".zip")
		if r.Code != 500 || r.Header().Get("X-Oberth-GoProxy-Error") != "pinned_source_missing" || r.Header().Get("Retry-After") != "" || g.failedArchives.Load() != attempt {
			t.Fatalf("permanent source failure was retried: %d %s archive calls=%d", r.Code, r.Body, g.failedArchives.Load())
		}
	}
	if r := artifactRequest(h, ".mod"); r.Code != 200 || r.Body.String() != artifactMod {
		t.Fatal("missing zip source changed pinned metadata", r.Code, r.Body)
	}
}

func TestRealGitPrunedPinnedCommitCannotRebindLazyZip(t *testing.T) {
	work := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	git(work, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(artifactMod), 0600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-m", "first")
	git(work, "tag", "v1.0.0")
	cache, err := gitcache.New(gitcache.Config{Root: t.TempDir(), Upstream: func(string) (string, error) { return work, nil }})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(cache, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithArtifactCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := artifactRequest(h, ".mod")
	if mod.Code != 200 {
		t.Fatal(mod.Body)
	}
	pinned, err := h.artifacts.lookup("example", artifactModule, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	git(work, "checkout", "--orphan", "replacement")
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(artifactMod+"// replacement tree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-m", "replacement")
	for _, ref := range strings.Fields(git(repo.Path, "for-each-ref", "--format=%(refname)")) {
		git(repo.Path, "update-ref", "-d", ref)
	}
	git(repo.Path, "fetch", "--force", "--no-tags", work, "refs/heads/replacement:refs/heads/main", "refs/heads/replacement:refs/tags/v1.0.0")
	git(repo.Path, "reflog", "expire", "--expire=now", "--all")
	git(repo.Path, "gc", "--prune=now")
	cmd := exec.CommandContext(t.Context(), "git", "-C", repo.Path, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(pinned.Source + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != pinned.Source+" missing" {
		t.Fatalf("fixture did not prune pinned source: %s %v", out, err)
	}
	r := artifactRequest(h, ".zip")
	if r.Code != 500 || r.Header().Get("X-Oberth-GoProxy-Error") != "pinned_source_missing" || r.Header().Get("Retry-After") != "" {
		t.Fatalf("pruned source remained retryable: %d %s", r.Code, r.Body)
	}
	after := artifactRequest(h, ".mod")
	if after.Code != 200 || !bytes.Equal(after.Body.Bytes(), mod.Body.Bytes()) {
		t.Fatal("pruned source rebound metadata to moved tag")
	}
}

func TestLatestDiscoveryRequiresRevalidationDespitePinnedInfo(t *testing.T) {
	g := newArtifactGit()
	h := artifactHandler(t, g, t.TempDir())
	first := proxyDiscovery(h, "/@latest")
	if first.Code != 200 || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("mutable latest cached as immutable: %d %v", first.Code, first.Header())
	}
	g.tags = append(g.tags, "v1.1.0")
	second := proxyDiscovery(h, "/@latest")
	var info struct{ Version string }
	if err := json.Unmarshal(second.Body.Bytes(), &info); err != nil || second.Code != 200 || info.Version != "v1.1.0" || first.Header().Get("ETag") == second.Header().Get("ETag") {
		t.Fatalf("latest did not discover a new tag: %d %s", second.Code, second.Body)
	}
	explicit := proxyDiscovery(h, "/@v/v1.1.0.info")
	if explicit.Code != 200 || !strings.Contains(explicit.Header().Get("Cache-Control"), "immutable") || !bytes.Equal(explicit.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("explicit info lost immutable cache policy or pin")
	}
}
