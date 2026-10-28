package gitcache

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShadowExportAttributeGapsAreNotMismatches(t *testing.T) {
	for _, kind := range []string{"export-ignore", "export-subst", "nested", "info", "configured", "unused", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			repository := newTestRepository(t)
			target, body := ".gitattributes", "file.txt "+kind+"\n"
			if kind == "unused" {
				body = "nonexistent export-ignore\n"
			}
			if kind == "nested" {
				if err := os.Mkdir(filepath.Join(repository.work, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				target, body = "nested/.gitattributes", "* export-ignore\n"
				if err := os.WriteFile(filepath.Join(repository.work, "nested", "file"), []byte("source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "export-subst" {
				body = "README.md export-subst\n"
				if err := os.WriteFile(filepath.Join(repository.work, "README.md"), []byte("$Format:%H$\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "info" && kind != "configured" && kind != "symlink" {
				if err := os.WriteFile(filepath.Join(repository.work, filepath.FromSlash(target)), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repository.work, "add", ".")
				runGit(t, repository.work, "commit", "-m", "export attributes")
				runGit(t, repository.work, "push", "origin", "HEAD:refs/heads/main")
			}
			if kind == "symlink" {
				// git archive reads the committed symlink blob as attribute text,
				// not its target in the checkout (unlike working-tree attr lookup).
				if err := os.Symlink("file.txt export-ignore", filepath.Join(repository.work, ".gitattributes")); err != nil {
					t.Fatal(err)
				}
				runGit(t, repository.work, "add", ".gitattributes")
				runGit(t, repository.work, "commit", "-m", "symlink attributes")
				runGit(t, repository.work, "push", "origin", "HEAD:refs/heads/main")
			}
			var logs shadowTestLog
			cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Shadow: true, ShadowContext: t.Context(), Upstream: func(string) (string, error) { return repository.upstream, nil }, Logger: log.New(&logs, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cache.CloseShadow)
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(cache.root, "example.git")
			if kind == "info" {
				if err := os.WriteFile(filepath.Join(repo, "info", "attributes"), []byte("file.txt export-ignore\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "configured" {
				attr := filepath.Join(t.TempDir(), "attributes")
				if err := os.WriteFile(attr, []byte("file.txt export-ignore\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "config", "core.attributesFile", attr)
			}
			sha := runGit(t, repository.work, "rev-parse", "HEAD")
			var expected, actual bytes.Buffer
			if err := cache.run(t.Context(), commandSpec{dir: repo, args: []string{"archive", "--format=tar", sha}, stdout: &expected}); err != nil {
				t.Fatal(err)
			}
			if err := cache.ArchiveTar(t.Context(), "example", sha, &actual); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(expected.Bytes(), actual.Bytes()) {
				t.Fatal("attribute gap changed authoritative CLI delivery")
			}
			if kind == "export-subst" {
				archive := tar.NewReader(bytes.NewReader(actual.Bytes()))
				expanded := false
				for {
					h, err := archive.Next()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if h.Name == "README.md" {
						raw, err := io.ReadAll(archive)
						if err != nil {
							t.Fatal(err)
						}
						expanded = string(raw) == sha+"\n"
					}
				}
				if !expanded {
					t.Fatal("fixture did not exercise Git substitution")
				}
			}
			waitTestShadow(t, cache)
			requireShadowMetric(t, cache, "oberth_gitcache_shadow_compared_total", 1)
			wantErrors := uint64(1)
			if kind == "unused" || kind == "configured" {
				wantErrors = 0
			}
			if stats := cache.ShadowStats(); stats.Runs != 1 || stats.Errors != wantErrors || stats.Mismatches != 0 {
				t.Fatalf("gap counted as parity: %+v %s", stats, logs.String())
			}
			var metrics bytes.Buffer
			cache.WriteShadowMetrics(&metrics)
			if !strings.Contains(metrics.String(), fmt.Sprintf(`oberth_gitcache_shadow_errors_total{reason="expected_attr_gap"} %d`, wantErrors)) {
				t.Fatal(metrics.String())
			}
			// Direct go-git opt-in must also fail closed instead of returning a wrong archive.
			r, closeStorage, err := openGoRepository(repo)
			if err != nil {
				t.Fatal(err)
			}
			defer closeStorage()
			if err := goArchive(t.Context(), r, sha, io.Discard); !errors.Is(err, errArchiveAttrGap) {
				t.Fatalf("unsupported attributes silently ignored: %v", err)
			}
		})
	}
}

func TestArchiveExportRuleDetection(t *testing.T) {
	for _, test := range []struct {
		body string
		gap  bool
	}{
		{"# export-ignore\n*.go text eol=lf\nexport-ignore text\nfile -export-ignore !export-subst\n", false},
		{"file export-ignore\n", true}, {"file\texport-subst\n", true},
		{"[attr]release export-ignore\nfile release\n", true},
		{`"path with spaces" export-subst` + "\n", true},
		{`"escaped\\\"pattern" export-ignore` + "\n", true},
		{`""` + "\n", false},
		{"file\vexport-ignore\n", false}, {"file\fexport-subst\n", false},
		{"file\u00a0export-ignore\n", false}, {"file export-ignore\u00a0text\n", false},
		{"file\rexport-ignore\n", true},
	} {
		got, err := scanArchiveExportRules(strings.NewReader(test.body))
		if err != nil || got != test.gap {
			t.Fatalf("%q: gap=%v err=%v", test.body, got, err)
		}
	}
}
