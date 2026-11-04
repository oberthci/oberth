package goproxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/oberthci/oberth/internal/gitcache"
)

type nestedOrigin struct{ VCS, URL, Hash, Ref, Subdir string }

func TestNestedModuleVersionsAndOrigin(t *testing.T) {
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
	write := func(name, body string) {
		t.Helper()
		name = filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "init", "-b", "main")
	for name, body := range map[string]string{
		"go.mod": artifactMod, "root.go": "package root\n",
		"LICENSE":                  "repository license\n",
		"subdir/go.mod":            "module " + artifactModule + "/subdir\n\ngo 1.22\n",
		"subdir/module.go":         "package subdir\n// tagged\n",
		"subdir/helper/helper.go":  "package helper\n",
		"subdir/child/go.mod":      "module " + artifactModule + "/subdir/child\n\ngo 1.22\n",
		"subdir/child/child.go":    "package child\n",
		"subdir/vendor/dep/dep.go": "package dep\n",
		"sibling/go.mod":           "module " + artifactModule + "/sibling\n\ngo 1.22\n",
		"sibling/module.go":        "package sibling\n",
		"sibling/LICENSE":          "sibling license\n",
		"symlinklicense/go.mod":    "module " + artifactModule + "/symlinklicense\n\ngo 1.22\n",
		"symlinklicense/module.go": "package symlinklicense\n",
		"deep/more/go.mod":         "module " + artifactModule + "/deep/more\n\ngo 1.22\n",
		"deep/more/module.go":      "package deep\n",
		"major/go.mod":             "module " + artifactModule + "/major/v2\n\ngo 1.22\n",
		"major/module.go":          "package major\n",
		"versioned/v2/go.mod":      "module " + artifactModule + "/versioned/v2\n\ngo 1.22\n",
		"versioned/v2/module.go":   "package versioned\n",
		"ambiguous/go.mod":         "module " + artifactModule + "/ambiguous/v2\n\ngo 1.22\n",
		"ambiguous/v2/go.mod":      "module " + artifactModule + "/ambiguous/v2\n\ngo 1.22\n",
		"wrong/go.mod":             "module example.invalid/wrong\n\ngo 1.22\n",
		"subdir-sibling/secret.go": "package secret\n",
	} {
		write(name, body)
	}
	if err := os.Symlink("../root.go", filepath.Join(work, "subdir/link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../LICENSE", filepath.Join(work, "symlinklicense/LICENSE")); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-m", "nested module base")
	base := git(work, "rev-parse", "HEAD")
	git(work, "tag", "v1.0.0")
	git(work, "tag", "-a", "subdir/v1.2.3", "-m", "nested release")
	git(work, "tag", "sibling/v1.2.3")
	git(work, "tag", "subdir-sibling/v1.9.9")
	git(work, "tag", "wrong/v1.2.3")
	git(work, "tag", "deep/more/v1.2.3")
	git(work, "tag", "major/v2.0.0")
	git(work, "tag", "versioned/v2.0.0")
	git(work, "tag", "symlinklicense/v1.2.3")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T07:08:08Z")
	write("subdir/module.go", "package subdir\n// pseudo\n")
	git(work, "commit", "-am", "nested module on main")
	source := git(work, "rev-parse", "HEAD")
	when := time.Date(2026, 10, 1, 7, 8, 8, 0, time.UTC)
	pseudo := module.PseudoVersion("v0", "", when, source[:12])
	derived := module.PseudoVersion("v1", "v1.2.3", when, source[:12])
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
	request := func(mod, version, ext string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/"+mod+"/@v/"+version+ext, nil))
		return r
	}
	for _, tc := range []struct{ subdir, originDir, version, hash, ref, body string }{
		{"subdir", "subdir", pseudo, source, "", "package subdir\n// pseudo\n"},
		{"subdir", "subdir", derived, source, "", "package subdir\n// pseudo\n"},
		{"subdir", "subdir", "v1.2.3", base, "refs/tags/subdir/v1.2.3", "package subdir\n// tagged\n"},
		{"sibling", "sibling", "v1.2.3", base, "refs/tags/sibling/v1.2.3", "package sibling\n"},
		{"symlinklicense", "symlinklicense", "v1.2.3", base, "refs/tags/symlinklicense/v1.2.3", "package symlinklicense\n"},
		{"deep/more", "deep/more", "v1.2.3", base, "refs/tags/deep/more/v1.2.3", "package deep\n"},
		{"major/v2", "major", "v2.0.0", base, "refs/tags/major/v2.0.0", "package major\n"},
		{"versioned/v2", "versioned", "v2.0.0", base, "refs/tags/versioned/v2.0.0", "package versioned\n"},
		{"major/v2", "major", module.PseudoVersion("v2", "", when, source[:12]), source, "", "package major\n"},
	} {
		t.Run(tc.subdir+"/"+tc.version, func(t *testing.T) {
			mod := artifactModule + "/" + tc.subdir
			wantMod := "module " + mod + "\n\ngo 1.22\n"
			info := request(mod, tc.version, ".info")
			wantOrigin := nestedOrigin{"git", work, tc.hash, tc.ref, tc.originDir}
			var result struct{ Origin nestedOrigin }
			if info.Code != 200 || json.Unmarshal(info.Body.Bytes(), &result) != nil || result.Origin != wantOrigin {
				t.Fatalf("nested info: %d %s; want Origin %+v", info.Code, info.Body, wantOrigin)
			}
			gomod := request(mod, tc.version, ".mod")
			if gomod.Code != 200 || gomod.Body.String() != wantMod {
				t.Fatalf("nested go.mod: %d %s", gomod.Code, gomod.Body)
			}
			// Move the tag after metadata publication, leaving the zip cold.
			if tc.subdir == "subdir" && tc.ref != "" {
				git(repo.Path, "update-ref", tc.ref, source)
			}
			f := newResolverFixture(t, h, http.StatusNotFound)
			f.proxy = strings.Split(f.proxy, ",")[0]
			output, err := f.run(t, "mod", "download", "-json", mod+"@"+tc.version)
			var download struct {
				Origin             nestedOrigin
				Sum, GoModSum, Zip string
			}
			if err != nil || json.Unmarshal([]byte(output), &download) != nil || download.Origin != wantOrigin || download.Sum == "" || download.GoModSum == "" {
				t.Fatalf("nested Go download: %v %s", err, output)
			}
			wantFiles := map[string]string{"go.mod": wantMod, "module.go": tc.body, "LICENSE": "repository license\n"}
			if tc.subdir == "sibling" {
				wantFiles["LICENSE"] = "sibling license\n"
			}
			if tc.subdir == "symlinklicense" {
				delete(wantFiles, "LICENSE")
			}
			if tc.subdir == "subdir" {
				wantFiles["helper/helper.go"] = "package helper\n"
			}
			archive, err := zip.OpenReader(download.Zip)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = archive.Close() }()
			if len(archive.File) != len(wantFiles) {
				t.Fatalf("zip includes files outside the module: %v", archive.File)
			}
			for _, entry := range archive.File {
				name := strings.TrimPrefix(entry.Name, mod+"@"+tc.version+"/")
				file, err := entry.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				want, ok := wantFiles[name]
				if err != nil || !ok || string(data) != want {
					t.Fatalf("zip file %q = %q: %v", name, data, err)
				}
			}
			dir := t.TempDir()
			for name, body := range wantFiles {
				target := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var canonical bytes.Buffer
			if err := modzip.CreateFromDir(&canonical, module.Version{Path: mod, Version: tc.version}, dir); err != nil {
				t.Fatal(err)
			}
			zipped := request(mod, tc.version, ".zip")
			if zipped.Code != 200 || !bytes.Equal(zipped.Body.Bytes(), canonical.Bytes()) {
				t.Fatal("nested zip differs from canonical module bytes")
			}
			output, err = f.run(t, "list", "-m", "-json", mod+"@"+tc.version)
			if err != nil || json.Unmarshal([]byte(output), &result) != nil || result.Origin != wantOrigin {
				t.Fatalf("nested Go list Origin: %v %s", err, output)
			}
			h = newHandler()
			repeated := request(mod, tc.version, ".info")
			if repeated.Code != 200 || !bytes.Equal(info.Body.Bytes(), repeated.Body.Bytes()) || info.Header().Get("ETag") != repeated.Header().Get("ETag") {
				t.Fatal("nested Origin changed after restart or tag movement")
			}
		})
	}
	for _, tc := range []struct{ subdir, version string }{
		{"subdir", module.PseudoVersion("v0", "", when, "ffffffffffff")},
		{"subdir", module.PseudoVersion("v1", "v1.1.0", when, source[:12])},
		{"missing", pseudo}, {"wrong", pseudo}, {"wrong", "v1.2.3"},
		{"subdir", "v1.0.0"}, {"subdir/helper", pseudo},
		{"ambiguous/v2", module.PseudoVersion("v2", "", when, source[:12])},
	} {
		for _, ext := range []string{".info", ".mod", ".zip"} {
			r := request(artifactModule+"/"+tc.subdir, tc.version, ext)
			if r.Code != http.StatusForbidden {
				t.Fatalf("invalid nested %s@%s%s: %d %s", tc.subdir, tc.version, ext, r.Code, r.Body)
			}
		}
	}
	list := request(artifactModule+"/subdir", "list", "")
	if list.Code != 200 || list.Body.String() != "v1.2.3\n" {
		t.Fatalf("nested tag discovery mixed tag namespaces: %d %s", list.Code, list.Body)
	}
	latest := httptest.NewRecorder()
	h.ServeHTTP(latest, httptest.NewRequest("GET", "/"+artifactModule+"/subdir/@latest", nil))
	if latest.Code != 200 || !bytes.Contains(latest.Body.Bytes(), []byte(`"Version":"v1.2.3"`)) {
		t.Fatalf("nested latest: %d %s", latest.Code, latest.Body)
	}
	f := newResolverFixture(t, h, http.StatusNotFound)
	unknown := module.PseudoVersion("v0", "", when, "ffffffffffff")
	output, err := f.run(t, "mod", "download", artifactModule+"/subdir@"+unknown)
	if err == nil || !strings.Contains(output, "403") || len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("unknown nested commit escaped the mirror: %v %s", err, output)
	}
	// Even warm nested artifacts need fresh default-branch authority.
	warm := request(artifactModule+"/subdir", pseudo, ".info")
	git(repo.Path, "update-ref", "refs/heads/main", base)
	for _, ext := range []string{".info", ".mod", ".zip"} {
		if r := request(artifactModule+"/subdir", pseudo, ext); r.Code != http.StatusForbidden {
			t.Fatalf("unreachable nested commit served from cache: %d %s", r.Code, r.Body)
		}
	}
	git(repo.Path, "update-ref", "refs/heads/main", source)
	if restored := request(artifactModule+"/subdir", pseudo, ".info"); restored.Code != 200 || !bytes.Equal(warm.Body.Bytes(), restored.Body.Bytes()) {
		t.Fatal("restoring nested eligibility changed immutable Origin")
	}
	// A repository need not contain a root module at all.
	git(work, "rm", "go.mod")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T07:08:09Z")
	git(work, "commit", "-m", "nested modules only")
	only := git(work, "rev-parse", "HEAD")
	git(repo.Path, "fetch", work, "main:refs/heads/main")
	onlyVersion := module.PseudoVersion("v0", "", when.Add(time.Second), only[:12])
	for _, ext := range []string{".info", ".mod", ".zip"} {
		if r := request(artifactModule+"/subdir", onlyVersion, ext); r.Code != 200 {
			t.Fatalf("nested module without root go.mod: %d %s", r.Code, r.Body)
		}
	}
}
