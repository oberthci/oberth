package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestRealGitModuleOriginMatchesPinnedZip(t *testing.T) {
	for _, kind := range []string{"lightweight", "annotated", "pseudo"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GIT_AUTHOR_DATE", "2026-10-01T07:08:07Z")
			t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T07:08:07Z")
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
			for name, data := range map[string]string{"go.mod": artifactMod, "source.txt": "original commit\n"} {
				if err := os.WriteFile(filepath.Join(work, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(work, "add", ".")
			git(work, "commit", "-m", "module source")
			source := git(work, "rev-parse", "HEAD")
			version, ref := "v1.0.0", "refs/tags/v1.0.0"
			switch kind {
			case "lightweight":
				git(work, "tag", version)
			case "annotated":
				git(work, "tag", "-a", version, "-m", "release")
				if git(work, "rev-parse", ref) == source {
					t.Fatal("fixture needs a distinct annotated tag object")
				}
			case "pseudo":
				version = module.PseudoVersion("v0", "", time.Date(2026, 10, 1, 7, 8, 7, 0, time.UTC), source[:12])
				ref = ""
			}
			cache, err := gitcache.New(gitcache.Config{AllowLocalUpstreams: true, Root: t.TempDir(), Upstream: func(string) (string, error) { return work, nil }})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			newHandler := func() *Handler {
				h, err := NewHandler(cache, "go.example.test", map[string]string{"issue764-fixture": "example"}).WithArtifactCache(root)
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			h := newHandler()
			infoResponse := pseudoRequest(h, version, ".info")
			var info moduleInfo
			want := moduleOrigin{VCS: "git", URL: work, Hash: source, Ref: ref}
			if infoResponse.Code != 200 || json.Unmarshal(infoResponse.Body.Bytes(), &info) != nil || info.Version != version || info.Origin != want {
				t.Fatalf(".info: %d %s; want Origin %+v", infoResponse.Code, infoResponse.Body, want)
			}
			if kind == "pseudo" && bytes.Contains(infoResponse.Body.Bytes(), []byte(`"Ref"`)) {
				t.Fatal("pseudo-version Origin must omit Ref")
			}
			// Metadata is already published; the zip has yet to be built. Move
			// the tag to another tree and change the remote before restarting.
			if ref != "" {
				if err := os.WriteFile(filepath.Join(work, "source.txt"), []byte("moved commit\n"), 0600); err != nil {
					t.Fatal(err)
				}
				git(work, "commit", "-am", "move release")
				moved := git(work, "rev-parse", "HEAD")
				git(repo.Path, "fetch", work, "trunk")
				git(repo.Path, "update-ref", ref, moved)
			}
			git(repo.Path, "remote", "set-url", "upstream", "ssh://git@changed.example.test/other/repository.git")
			h = newHandler()
			f := newResolverFixture(t, h, http.StatusNotFound)
			// Exercise an actual mirror-only Go resolver with independent cold
			// module caches for download and list.
			f.proxy = strings.Split(f.proxy, ",")[0]
			for _, args := range [][]string{{"mod", "download", "-json", artifactModule + "@" + version}, {"list", "-m", "-json", artifactModule + "@" + version}} {
				output, err := f.run(t, args...)
				var result struct {
					Origin moduleOrigin
					Zip    string
				}
				if err != nil || json.Unmarshal([]byte(output), &result) != nil || result.Origin != want {
					t.Fatalf("Go Origin: %v %s; want %+v", err, output, want)
				}
				if result.Zip != "" {
					archive, err := zip.OpenReader(result.Zip)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = archive.Close() }()
					file, err := archive.Open(artifactModule + "@" + version + "/source.txt")
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(file)
					_ = file.Close()
					if err != nil || string(data) != "original commit\n" {
						t.Fatalf("zip followed moved tag: %q %v", data, err)
					}
				}
			}
			entry, err := h.artifacts.lookupStage("example", artifactModule, version, ".zip")
			if err != nil || entry == nil || entry.Source != info.Origin.Hash {
				t.Fatalf("zip source disagrees with Origin: %+v %v", entry, err)
			}
			repeated := pseudoRequest(h, version, ".info")
			if repeated.Code != 200 || !bytes.Equal(infoResponse.Body.Bytes(), repeated.Body.Bytes()) || repeated.Header().Get("ETag") != infoResponse.Header().Get("ETag") {
				t.Fatal("published Origin changed after restart, tag move or upstream change")
			}
			if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
				t.Fatal("mirror-only resolver contacted fallback")
			}
		})
	}
}

func TestLegacyInfoRemainsImmutable(t *testing.T) {
	g := newArtifactGit()
	h := artifactHandler(t, g, t.TempDir())
	legacy := []byte("{\"Version\":\"v1.0.0\",\"Time\":\"2026-09-29T12:00:00Z\"}\n")
	_, err := h.artifacts.get(t.Context(), "forge/owner/repository", artifactModule, "v1.0.0", "", time.Second, func(_ context.Context) (string, map[string][]byte, error) {
		return artifactOID, map[string][]byte{".info": legacy, ".mod": []byte(artifactMod)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g.down.Store(true)
	r := artifactRequest(h, ".info")
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), legacy) || g.calls.Load() != 0 {
		t.Fatalf("legacy info regenerated: %d %s", r.Code, r.Body)
	}
}
