package goproxy

import (
	"archive/zip"
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

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestNestedLicenseMatchesDirectGo(t *testing.T) {
	const mod = "github.com/oberthci/license-fixture/subdir"
	for _, tc := range []struct {
		name, root, local, want string
		present                 bool
	}{
		{"root-symlink", "symlink", "", "COPYING", true},
		{"root-dangling-symlink", "dangling", "", "ABSENT", true},
		{"root-regular", "regular", "", "repository license\n", true},
		{"root-missing", "", "", "", false},
		{"root-directory", "directory", "", "", false},
		{"root-export-ignore", "export-ignore", "", "repository license\n", true},
		{"local-regular", "symlink", "regular", "module license\n", true},
		{"local-symlink", "regular", "symlink", "", false},
		{"empty-root", "empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			write := func(name, body string) {
				t.Helper()
				target := filepath.Join(work, name)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			link := func(name, target string) {
				t.Helper()
				if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-b", "main")
			write("subdir/go.mod", "module "+mod+"\n\ngo 1.22\n")
			write("subdir/module.go", "package subdir\n")
			switch tc.root {
			case "symlink":
				write("COPYING", "must not follow this link\n")
				link("LICENSE", "COPYING")
			case "dangling":
				link("LICENSE", "ABSENT")
			case "regular", "export-ignore":
				write("LICENSE", "repository license\n")
				if tc.root == "export-ignore" {
					write(".gitattributes", "LICENSE export-ignore\n")
				}
			case "directory":
				write("LICENSE/file", "not a blob\n")
			case "empty":
				write("LICENSE", "")
			}
			switch tc.local {
			case "regular":
				write("subdir/LICENSE", "module license\n")
			case "symlink":
				link("subdir/LICENSE", "../LICENSE")
			}
			git("add", ".")
			git("commit", "-m", "license fixture")
			git("tag", "subdir/v1.0.0")

			// Real direct resolution, with only this exact GitHub repository rewritten
			// to a local fixture. No network protocol or ambient Git config is allowed.
			direct := t.TempDir()
			config := filepath.Join(direct, "gitconfig")
			if err := os.WriteFile(config, []byte("[url \""+work+"\"]\n\tinsteadOf = https://github.com/oberthci/license-fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(direct, "go.mod"), []byte("module fixture.invalid/root\n\ngo 1.22\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(30*time.Second))
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", mod+"@v1.0.0")
			cmd.Dir = direct
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + direct, "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOMAXPROCS=2", "GOFLAGS=-p=2 -modcacherw",
				"GOPROXY=direct", "GOSUMDB=off", "GOTELEMETRY=off", "GOPATH=" + filepath.Join(direct, "gopath"), "GOMODCACHE=" + filepath.Join(direct, "gomod"), "GOCACHE=" + filepath.Join(direct, "gocache"),
				"GIT_CONFIG_GLOBAL=" + config, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file", "GOVCS=*:git",
				"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9",
			}
			out, err := cmd.CombinedOutput()
			var download struct{ Sum, GoModSum, Zip string }
			if err != nil || json.Unmarshal(out, &download) != nil || download.Sum == "" {
				t.Fatalf("direct Go: %v %s", err, out)
			}
			assertLicense := func(zipPath string) {
				t.Helper()
				archive, err := zip.OpenReader(zipPath)
				if err != nil {
					t.Fatal(err)
				}
				defer archive.Close()
				found := false
				for _, file := range archive.File {
					if file.Name != mod+"@v1.0.0/LICENSE" {
						continue
					}
					found = true
					if !file.Mode().IsRegular() {
						t.Fatal("inherited LICENSE must be regular")
					}
					reader, err := file.Open()
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(reader)
					reader.Close()
					if err != nil || string(body) != tc.want {
						t.Fatalf("LICENSE = %q; want %q: %v", body, tc.want, err)
					}
				}
				if found != tc.present {
					t.Fatalf("LICENSE present = %t; want %t", found, tc.present)
				}
			}
			assertLicense(download.Zip)
			cache, err := gitcache.New(gitcache.Config{Root: t.TempDir(), AllowLocalUpstreams: true, Upstream: func(string) (string, error) { return work, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			h, err := NewHandler(cache, "github.com/oberthci", map[string]string{"license-fixture": "example"}).WithArtifactCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			f := newResolverFixture(t, h, http.StatusNotFound)
			f.proxy = strings.Split(f.proxy, ",")[0]
			sums := mod + " v1.0.0 " + download.Sum + "\n" + mod + " v1.0.0/go.mod " + download.GoModSum + "\n"
			output, err := f.runWithSum(t, sums, "mod", "download", "-json", mod+"@v1.0.0")
			var mirror struct{ Sum, GoModSum, Zip string }
			if err != nil || json.Unmarshal([]byte(output), &mirror) != nil || mirror.Sum != download.Sum || mirror.GoModSum != download.GoModSum {
				t.Fatalf("mirror differs from direct Go with recorded go.sum: %v %s; direct checksum %s", err, output, download.Sum)
			}
			assertLicense(mirror.Zip)
			if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
				t.Fatal("mirror resolution escaped the fixture")
			}
			t.Logf("direct and mirror checksum %s", mirror.Sum)
		})
	}
}
