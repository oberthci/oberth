package goproxy

import (
	zipreader "archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
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
	"golang.org/x/mod/sumdb/dirhash"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestPublicOriginNativeTLSGo(t *testing.T) {
	work := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_DATE=2026-11-03T20:05:34Z", "GIT_COMMITTER_DATE=2026-11-03T20:05:34Z")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("local fixture git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(work, "init", "-b", "main")
	for name, body := range map[string]string{"go.mod": "module go.example.test/oberth-fixture\n\ngo 1.25.0\n", "fixture.go": "package fixture\n", "LICENSE": "fixture license\n", "nested/go.mod": "module go.example.test/oberth-fixture/nested\n\ngo 1.25.0\n", "nested/nested.go": "package nested\n"} {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "add", ".")
	git(work, "commit", "-m", "Oberth Origin fixture")
	source := git(work, "rev-parse", "HEAD")
	git(work, "tag", "-a", "v0.4.3", "-m", "fixture release")
	cache, err := gitcache.New(gitcache.Config{AllowLocalUpstreams: true, Root: t.TempDir(), Upstream: func(string) (string, error) { return work, nil }})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Ensure(t.Context(), "forge/oberth/oberth-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ssh := "ssh://git@codeberg.org/oberth/oberth-fixture.git"
	git(repo.Path, "remote", "set-url", "upstream", ssh)
	wrapped := &refreshingOriginCache{Cache: cache, refresh: func(context.Context) error {
		// Change the real remote after coherent snapshot capture. Metadata must
		// still identify the selected repository, without a second remote read.
		git(repo.Path, "remote", "set-url", "upstream", "ssh://git@codeberg.org/other/unrelated.git")
		return nil
	}}
	artifactRoot := t.TempDir()
	h, err := NewHandler(wrapped, "go.example.test", map[string]string{"oberth-fixture": "forge/oberth/oberth-fixture"}).WithArtifactCache(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Pointer[Handler]
	active.Store(h)
	var requested []string
	var requestsMu sync.Mutex
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 {
			t.Error("native request did not negotiate TLS 1.3")
		}
		requestsMu.Lock()
		requested = append(requested, r.URL.Path)
		requestsMu.Unlock()
		active.Load().ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	git(work, "tag", "nested/v0.4.3")
	git(repo.Path, "fetch", work, "refs/tags/nested/v0.4.3:refs/tags/nested/v0.4.3")
	when := time.Date(2026, 11, 3, 20, 5, 34, 0, time.UTC)
	for _, tc := range []struct{ label, mod, version, subdir, ref string }{{"root", "go.example.test/oberth-fixture", "v0.4.3", "", "refs/tags/v0.4.3"}, {"nested-tag", "go.example.test/oberth-fixture/nested", "v0.4.3", "nested", "refs/tags/nested/v0.4.3"}, {"nested-pseudo", "go.example.test/oberth-fixture/nested", module.PseudoVersion("v0", "", when, source[:12]), "nested", ""}} {
		t.Run(tc.label, func(t *testing.T) {
			git(repo.Path, "remote", "set-url", "upstream", ssh)
			wrapped.once = sync.Once{}
			h, err = NewHandler(wrapped, "go.example.test", map[string]string{"oberth-fixture": "forge/oberth/oberth-fixture"}).WithArtifactCache(artifactRoot)
			if err != nil {
				t.Fatal(err)
			}
			request := func(ext string) []byte {
				t.Helper()
				r := httptest.NewRecorder()
				h.ServeHTTP(r, httptest.NewRequest("GET", "/"+tc.mod+"/@v/"+tc.version+ext, nil))
				if r.Code != 200 {
					t.Fatalf("native %s: %d %s", ext, r.Code, r.Body)
				}
				return r.Body.Bytes()
			}
			info := request(".info")
			var metadata moduleInfo
			if err := json.Unmarshal(info, &metadata); err != nil || !metadata.Time.Equal(when) {
				t.Fatalf("snapshot time: %s %v", info, err)
			}
			mod := request(".mod")
			zip := request(".zip")
			zipPath := filepath.Join(t.TempDir(), "fixture.zip")
			if err := os.WriteFile(zipPath, zip, 0600); err != nil {
				t.Fatal(err)
			}
			sum, err := dirhash.HashZip(zipPath, dirhash.Hash1)
			if err != nil {
				t.Fatal(err)
			}
			modsum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(mod)), nil })
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			gomod := filepath.Join(dir, "gomod")
			body := fmt.Sprintf("module origin.invalid/check\n\ngo 1.26.0\n\nrequire %s %s\n", tc.mod, tc.version)
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			sums := fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n", tc.mod, tc.version, sum, tc.mod, tc.version, modsum)
			if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(sums), 0600); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(dir, "home"), "XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "GOMODCACHE=" + gomod, "GOCACHE=" + filepath.Join(dir, "build"), "GOPATH=" + filepath.Join(dir, "gopath"), "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly -modcacherw -p=2", "GOMAXPROCS=2", "GOPROXY=" + server.URL, "GONOPROXY=none", "GOPRIVATE=", "GONOSUMDB=go.example.test/*", "GOSUMDB=sum.golang.org", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GOTELEMETRY=off", "SSL_CERT_FILE=" + ca, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=", "GIT_SSH_COMMAND=/bin/false", "NETRC=/dev/null"}
			run := func(args ...string) []byte {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "go", args...)
				cmd.Dir = dir
				cmd.Env = env
				stdout, err := cmd.Output()
				if err != nil {
					var e *exec.ExitError
					if errors.As(err, &e) {
						t.Fatalf("native Go %v: %v %s", args, err, e.Stderr)
					}
					t.Fatal(err)
				}
				return stdout
			}
			run("telemetry", "off")
			active.Store(h)
			actual := run("mod", "download", "-json", tc.mod+"@"+tc.version)
			var got struct {
				Path, Version, Sum, GoModSum string
				Origin                       struct{ VCS, URL, Hash, Ref, Subdir string }
			}
			if err := json.Unmarshal(actual, &got); err != nil {
				t.Fatal(err)
			}
			if got.Path != tc.mod || got.Version != tc.version || got.Origin.VCS != "git" || got.Origin.URL != "https://codeberg.org/oberth/oberth-fixture.git" || got.Origin.Hash != source || got.Origin.Ref != tc.ref || got.Origin.Subdir != tc.subdir || got.Sum != sum || got.GoModSum != modsum {
				t.Fatalf("native origin/identity %+v", got)
			}
			verify := run("mod", "verify")
			baseline, err := NewHandler(cache, "go.example.test", map[string]string{"oberth-fixture": "forge/oberth/oberth-fixture"}).WithArtifactCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := cache.ModuleVersionSnapshot(t.Context(), "forge/oberth/oberth-fixture", tc.mod, tc.subdir, tc.version, false)
			if err != nil {
				t.Fatal(err)
			}
			_, objects, err := baseline.buildPinnedVersion(t.Context(), "forge/oberth/oberth-fixture", tc.mod, tc.version, snapshot)
			if err != nil || !bytes.Equal(objects[".mod"], mod) {
				t.Fatalf("mod parity: %v", err)
			}
			// Conversion is independent of the identity helper; use the pinned
			// archive path directly to check canonical ZIP and LICENSE parity.
			archive, err := baseline.buildZip(t.Context(), "forge/oberth/oberth-fixture", tc.mod, tc.version, source)
			if err != nil {
				t.Fatal(err)
			}
			baselineZip, err := io.ReadAll(archive)
			_ = archive.Close()
			if err != nil || !bytes.Equal(baselineZip, zip) {
				t.Fatalf("ZIP parity: %v", err)
			}
			reader, err := zipreader.NewReader(bytes.NewReader(zip), int64(len(zip)))
			if err != nil {
				t.Fatal(err)
			}
			license, err := reader.Open(tc.mod + "@" + tc.version + "/LICENSE")
			if err != nil {
				t.Fatal(err)
			}
			licenseBytes, err := io.ReadAll(license)
			_ = license.Close()
			if err != nil || string(licenseBytes) != "fixture license\n" {
				t.Fatalf("LICENSE: %q %v", licenseBytes, err)
			}
			before := request(".info")
			git(repo.Path, "remote", "set-url", "upstream", "ssh://git@codeberg.org/other/unrelated.git")
			h, err = NewHandler(cache, "go.example.test", map[string]string{"oberth-fixture": "forge/oberth/oberth-fixture"}).WithArtifactCache(artifactRoot)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, request(".info")) || !bytes.Equal(mod, request(".mod")) || !bytes.Equal(zip, request(".zip")) {
				t.Fatal("immutable metadata or archive changed across restart/remote change")
			}
			t.Logf("native Go %s@%s Origin=%+v both h1 match; verify=%s", tc.mod, tc.version, got.Origin, verify)

		})
	}
	// Changing transport registration to HTTPS is not a supported fix: the
	// unchanged static-Git upstream validator refuses it before cold metadata.
	if err := gitcache.ValidateUpstream("https://codeberg.org/oberth/oberth-fixture.git"); err == nil {
		t.Fatal("expected native HTTPS upstream refusal")
	} else {
		t.Logf("native HTTPS transport registration refused: %v", err)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(requested) != 9 {
		t.Fatalf("expected nine TLS artifact requests, got %v", requested)
	}
	for _, p := range requested {
		if !strings.HasPrefix(p, "/go.example.test/") {
			t.Fatalf("unexpected request: %s", p)
		}
	}
}
