package gitcache

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBackendArchiveParity(t *testing.T) {
	repository := newTestRepository(t)
	if err := os.Mkdir(filepath.Join(repository.work, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository.work, "nested/run"), []byte("#!/bin/sh\nprintf example\\n\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/run", filepath.Join(repository.work, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.work, "add", "nested/run", "link")
	runGit(t, repository.work, "commit", "-m", "archive fixture")
	runGit(t, repository.work, "push", "origin", "HEAD:refs/heads/main")
	type entry struct {
		Mode int64
		Type byte
		Link string
		Body string
	}
	var expected map[string]entry
	for _, backend := range []Backend{BackendCLI, BackendGoGit} {
		t.Run(string(backend), func(t *testing.T) {
			cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: backend, Upstream: func(string) (string, error) { return repository.upstream, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			if err := cache.ArchiveTar(t.Context(), "example", "main", &archive); err != nil {
				t.Fatal(err)
			}
			result := map[string]entry{}
			reader := tar.NewReader(&archive)
			for {
				header, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if header.Typeflag == tar.TypeXGlobalHeader {
					continue
				}
				body, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				result[header.Name] = entry{header.Mode, header.Typeflag, header.Linkname, string(body)}
			}
			if backend == BackendCLI {
				expected = result
			} else if !reflect.DeepEqual(result, expected) {
				t.Fatalf("archive differs from CLI:\n got %#v\nwant %#v", result, expected)
			}
		})
	}
}
