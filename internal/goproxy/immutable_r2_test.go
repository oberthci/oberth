package goproxy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestImmutableMetadataDoesNotBuildZip(t *testing.T) {
	g := newArtifactGit()
	// This tree cannot become a valid Go module zip (Windows reserved name).
	g.tarEntries = []tarEntry{{name: "go.mod", typeflag: tar.TypeReg, content: []byte(artifactMod)}, {name: "CON", typeflag: tar.TypeReg, content: []byte("bad zip name")}}
	h := artifactHandler(t, g, t.TempDir())
	for _, ext := range []string{".mod", ".info"} {
		if r := artifactRequest(h, ext); r.Code != 200 {
			t.Fatalf("metadata coupled to archive: %d %s", r.Code, r.Body)
		}
	}
	if g.archives.Load() != 0 {
		t.Fatal("metadata built unused zip")
	}
	if r := artifactRequest(h, ".zip"); r.Code != 500 {
		t.Fatalf("invalid zip fixture accepted: %d %s", r.Code, r.Body)
	}
	g.down.Store(true)
	for _, ext := range []string{".mod", ".info"} {
		if r := artifactRequest(h, ext); r.Code != 200 {
			t.Fatal("failed zip poisoned metadata", r.Code)
		}
	}
}

func TestSharedArtifactFillSurvivesFirstWaiterCancellation(t *testing.T) {
	h := artifactHandler(t, newArtifactGit(), t.TempDir())
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	build := func(ctx context.Context) (string, map[string][]byte, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
		return artifactOID, map[string][]byte{".info": []byte("info"), ".mod": []byte(artifactMod)}, nil
	}
	leaderCtx, cancel := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := h.artifacts.get(leaderCtx, "repo", artifactModule, "v1.0.0", "", time.Second, build)
		leader <- err
	}()
	<-started
	follower := make(chan error, 1)
	go func() {
		_, err := h.artifacts.get(t.Context(), "repo", artifactModule, "v1.0.0", "", time.Second, build)
		follower <- err
	}()
	cancel()
	if err := <-leader; err == nil {
		t.Fatal("leader cancellation ignored")
	}
	close(release)
	if err := <-follower; err != nil {
		t.Fatal("leader canceled follower's fill", err)
	}
	if calls.Load() != 1 {
		t.Fatal("follower regenerated shared fill")
	}
}

func TestImmutableManifestConflictCannotRebindVersion(t *testing.T) {
	root := t.TempDir()
	first := artifactHandler(t, newArtifactGit(), root).artifacts
	second := artifactHandler(t, newArtifactGit(), root).artifacts
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := second.get(t.Context(), "repo", artifactModule, "v1.0.0", "", time.Second, func(context.Context) (string, map[string][]byte, error) {
			close(entered)
			<-release
			return "different-source", map[string][]byte{".mod": []byte("different"), ".info": []byte("different")}, nil
		})
		result <- err
	}()
	<-entered
	_, err := first.get(t.Context(), "repo", artifactModule, "v1.0.0", "", time.Second, func(context.Context) (string, map[string][]byte, error) {
		return artifactOID, map[string][]byte{".mod": []byte(artifactMod), ".info": []byte("info")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("conflicting publisher succeeded")
	}
	entry, err := second.lookup("repo", artifactModule, "v1.0.0")
	if err != nil || entry.Source != artifactOID {
		t.Fatal("committed manifest replaced", err)
	}
}

func TestRealGitTagMoveDeletionAndExplicitRevocation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(artifactMod), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "first")
	git("tag", "v1.0.0")
	cache, err := gitcache.New(gitcache.Config{Root: t.TempDir(), Upstream: func(string) (string, error) { return work, nil }})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h, err := NewHandler(cache, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithArtifactCache(root)
	if err != nil {
		t.Fatal(err)
	}
	// Pin cheap metadata before the tag moves, but leave zip unbuilt.
	mod := artifactRequest(h, ".mod")
	if mod.Code != 200 {
		t.Fatal(mod.Body)
	}
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module go.example.test/changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "second")
	git("tag", "-f", "v1.0.0")
	cmd := exec.CommandContext(t.Context(), "git", "-C", repo.Path, "fetch", "--force", work, "refs/tags/v1.0.0:refs/tags/v1.0.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("move: %v %s", err, out)
	}
	wantZip := artifactRequest(h, ".zip")
	if wantZip.Code != 200 {
		t.Fatal(wantZip.Body)
	}
	archive, err := zip.NewReader(bytes.NewReader(wantZip.Body.Bytes()), int64(wantZip.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, file := range archive.File {
		if filepath.Base(file.Name) == "go.mod" {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || string(body) != artifactMod {
				t.Fatal("lazy zip followed moved tag", string(body), err)
			}
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("zip omitted pinned module")
	}
	cmd = exec.CommandContext(t.Context(), "git", "-C", repo.Path, "update-ref", "-d", "refs/tags/v1.0.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("delete: %v %s", err, out)
	}
	if r := artifactRequest(h, ".mod"); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), mod.Body.Bytes()) {
		t.Fatal("deletion rebound pinned metadata")
	}
	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest("GET", "/"+artifactModule+"/@v/list", nil))
	if list.Code != 200 || bytes.Contains(list.Body.Bytes(), []byte("v1.0.0")) {
		t.Fatal("deleted tag stayed listed", list.Body)
	}
	marker := filepath.Join(root, "revoked", artifactKey("example", artifactModule, "v1.0.0"))
	cmd = exec.CommandContext(t.Context(), "python3", "../../hack/goproxy-revoke.py", "--cache-root", root, "--repository", "example", "--module", artifactModule, "--version", "v1.0.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("revocation utility: %v %s", err, out)
	}
	// Idempotent invocation must also fsync an existing deny marker.
	cmd = exec.CommandContext(t.Context(), "python3", "../../hack/goproxy-revoke.py", "--cache-root", root, "--repository", "example", "--module", artifactModule, "--version", "v1.0.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repeat revocation: %v %s", err, out)
	}
	// Revocation survives restart and denies all explicit representations.
	h, err = NewHandler(cache, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithArtifactCache(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".mod", ".info", ".zip"} {
		if r := artifactRequest(h, ext); r.Code != http.StatusForbidden {
			t.Fatalf("revoked %s served: %d", ext, r.Code)
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if r := artifactRequest(h, ".zip"); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), wantZip.Body.Bytes()) {
		t.Fatal("removing deny marker rebound content")
	}
}

func TestArtifactCacheTightensExistingDirectoryPermissions(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "objects"), filepath.Join(root, "versions"), filepath.Join(root, "revoked")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0777); err != nil {
			t.Fatal(err)
		}
	}
	artifactHandler(t, newArtifactGit(), root)
	for _, path := range []string{root, filepath.Join(root, "objects"), filepath.Join(root, "versions"), filepath.Join(root, "revoked")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("existing private directory not secured", path, err)
		}
	}
}
