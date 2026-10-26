package gitcache

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoGitLocalFetchRejectsCorruptObjectFiles(t *testing.T) {
	for _, kind := range []string{"pack", "index", "loose"} {
		t.Run(kind, func(t *testing.T) {
			repository := newTestRepository(t)
			if kind == "loose" {
				hash := runGit(t, repository.upstream, "rev-parse", "HEAD:file.txt")
				var body bytes.Buffer
				writer := zlib.NewWriter(&body)
				if _, err := writer.Write([]byte("blob 4\x00evil")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(repository.upstream, "objects", hash[:2], hash[2:])
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				runGit(t, repository.upstream, "repack", "-ad")
				extension := ".pack"
				if kind == "index" {
					extension = ".idx"
				}
				files, err := filepath.Glob(filepath.Join(repository.upstream, "objects/pack/pack-*"+extension))
				if err != nil || len(files) != 1 {
					t.Fatalf("pack files=%v %v", files, err)
				}
				body, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				body[20] ^= 1
				if err := os.Chmod(files[0], 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(files[0], body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Join(t.TempDir(), "cache")
			cache, err := New(Config{Root: root, Backend: BackendGoGit, Upstream: func(string) (string, error) { return repository.upstream, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Ensure(t.Context(), "example"); err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("corrupt %s accepted: %v", kind, err)
			}
			if _, err := os.Stat(filepath.Join(root, "example.git")); !os.IsNotExist(err) {
				t.Fatalf("corrupt initial cache published: %v", err)
			}
		})
	}
}
