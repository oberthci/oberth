package goproxy

import (
	"context"
	"encoding/json"
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

// Refresh exactly after commit selection, before the handler can publish any
// metadata. The refresh uses the real cache, repository lock and upstream swap.
type refreshingOriginCache struct {
	*gitcache.Cache
	once       sync.Once
	refresh    func(context.Context) error
	refreshErr error
}

func (c *refreshingOriginCache) selected(ctx context.Context) error {
	c.once.Do(func() {
		done := make(chan error, 1)
		go func() { done <- c.refresh(ctx) }()
		c.refreshErr = <-done
	})
	return c.refreshErr
}
func (c *refreshingOriginCache) ModuleVersionSnapshot(ctx context.Context, repo, mod, subdir, version string, allow bool) (gitcache.ModuleSnapshot, error) {
	snapshot, err := c.Cache.ModuleVersionSnapshot(ctx, repo, mod, subdir, version, allow)
	if err == nil {
		err = c.selected(ctx)
	}
	return snapshot, err
}

func TestColdOriginRemainsCoherentAcrossConcurrentRefresh(t *testing.T) {
	for _, subdir := range []string{"", "subdir"} {
		for _, kind := range []string{"lightweight", "annotated", "pseudo"} {
			t.Run(subdir+"/"+kind, func(t *testing.T) {
				t.Setenv("GIT_AUTHOR_DATE", "2026-10-01T07:08:07Z")
				t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T07:08:07Z")
				git := func(dir string, args ...string) string {
					t.Helper()
					cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
					return strings.TrimSpace(string(out))
				}
				mod := artifactModule
				tag := "v1.0.0"
				if subdir != "" {
					mod += "/" + subdir
					tag = subdir + "/" + tag
				}
				makeRepo := func(body string) (string, string) {
					t.Helper()
					work := t.TempDir()
					git(work, "init", "-b", "main")
					dir := filepath.Join(work, subdir)
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+mod+"\n\ngo 1.22\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					git(work, "add", ".")
					git(work, "commit", "-m", body)
					if kind == "annotated" {
						git(work, "tag", "-a", tag, "-m", "release")
					} else {
						git(work, "tag", tag)
					}
					return work, git(work, "rev-parse", "HEAD")
				}
				oldRepo, source := makeRepo("old source")
				otherRepo, otherSource := makeRepo("unrelated source")
				if source == otherSource {
					t.Fatal("fixture must use unrelated commits")
				}
				var upstream atomic.Value
				upstream.Store(oldRepo)
				cache, err := gitcache.New(gitcache.Config{Root: t.TempDir(), AllowLocalUpstreams: true, Upstream: func(string) (string, error) { return upstream.Load().(string), nil }})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := cache.Ensure(t.Context(), "example"); err != nil {
					t.Fatal(err)
				}
				wrapped := &refreshingOriginCache{Cache: cache, refresh: func(ctx context.Context) error {
					upstream.Store(otherRepo)
					_, err := cache.Ensure(ctx, "example")
					return err
				}}
				h, err := NewHandler(wrapped, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithArtifactCache(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				version, ref := "v1.0.0", "refs/tags/"+tag
				if kind == "pseudo" {
					version = module.PseudoVersion("v0", "", time.Date(2026, 10, 1, 7, 8, 7, 0, time.UTC), source[:12])
					ref = ""
				}
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest("GET", "/"+mod+"/@v/"+version+".info", nil))
				// A pseudo-version can be refused by its final live reachability check.
				// Even then it must not leave poisoned immutable metadata for later use.
				if response.Code != http.StatusOK && response.Code != http.StatusForbidden {
					t.Fatalf("response: %d %s", response.Code, response.Body)
				}
				entry, err := h.artifacts.lookupStage("example", mod, version, "")
				if err != nil {
					t.Fatal(err)
				}
				if entry == nil {
					t.Fatal("fixture did not reach metadata publication")
				}
				data, err := os.ReadFile(h.artifacts.objectPath(entry.Objects[".info"].SHA256))
				if err != nil {
					t.Fatal(err)
				}
				var info moduleInfo
				if err := json.Unmarshal(data, &info); err != nil {
					t.Fatal(err)
				}
				want := moduleOrigin{VCS: "git", URL: oldRepo, Hash: source, Ref: ref, Subdir: subdir}
				if info.Origin != want {
					t.Fatalf("incoherent cold-fill Origin: %+v; want %+v", info.Origin, want)
				}
				if kind != "pseudo" && response.Code != http.StatusOK {
					t.Fatalf("tag refused: %d %s", response.Code, response.Body)
				}
				t.Logf("%s: HTTP %d, pinned Origin %s %s", kind, response.Code, info.Origin.URL, info.Origin.Hash)
			})
		}
	}
}
