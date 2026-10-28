package gitcache_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/gitcache"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("fixture git %s: %w: %s", args[0], err, stderr.String())
	}
	return out, nil
}
func TestArchiveRealHistory(t *testing.T) {
	repository := os.Getenv("OBERTH_ARCHIVE_PARITY_REPO")
	if repository == "" {
		t.Skip("set OBERTH_ARCHIVE_PARITY_REPO=fixture for the full offline history")
	}
	if repository == "fixture" {
		repository = extractHistoryFixture(t)
	}
	root := t.TempDir()
	if err := os.Symlink(repository, filepath.Join(root, "mirror.git")); err != nil {
		t.Fatal(err)
	}
	cache, err := gitcache.New(gitcache.Config{Root: root, Upstream: func(string) (string, error) { return "", errors.New("offline fixture must not fetch") }})
	if err != nil {
		t.Fatal(err)
	}
	body, err := fixtureGit(t.Context(), repository, "rev-list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	commits := strings.Fields(string(body))
	if len(commits) == 0 {
		t.Fatal("empty history")
	}
	for i, commit := range commits {
		var actual bytes.Buffer
		if err := cache.ArchiveTar(t.Context(), "mirror", commit, &actual); err != nil {
			t.Fatalf("commit=%s: %v", commit, err)
		}
		expected, err := fixtureGit(t.Context(), repository, "archive", "--format=tar", commit)
		if err != nil || !bytes.Equal(actual.Bytes(), expected) {
			t.Fatalf("commit=%s archive differs: %v", commit, err)
		}
		if i%100 == 0 || i == len(commits)-1 {
			t.Logf("archived all files at %d/%d commits", i+1, len(commits))
		}
	}
}
func extractHistoryFixture(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/gengo-multipack.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != "f6aa30b3c2a8b2b291773eba1bab53ae29ee3471bf4a5877256caecaf7c53fc7" {
		t.Fatal("real-history fixture checksum changed; review provenance")
	}
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	compressor, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	archive := tar.NewReader(compressor)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(header.Name, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			t.Fatalf("unexpected fixture entry: %v", header)
		}
		if err := root.MkdirAll(filepath.Dir(header.Name), 0700); err != nil {
			t.Fatal(err)
		}
		file, err := root.OpenFile(header.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(file, archive)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			t.Fatal(err)
		}
	}
	packs, err := filepath.Glob(filepath.Join(directory, "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 3 {
		t.Fatalf("need three packs, got %v: %v", packs, err)
	}
	if _, err := fixtureGit(t.Context(), directory, "fsck", "--full"); err != nil {
		t.Fatal(err)
	}
	count, err := fixtureGit(t.Context(), directory, "rev-list", "--all", "--count")
	if err != nil || strings.TrimSpace(string(count)) != "878" {
		t.Fatalf("fixture history changed: %s %v", count, err)
	}
	return directory
}

// Production seeds the checked-out workspace, so archive-only parity is not
// enough. This real commit also corrupts with the released streaming decoder.
func TestCheckoutRealDeltaParity(t *testing.T) {
	repository := extractHistoryFixture(t)
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repository, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(t.Context(), "mirror"); err != nil {
		t.Fatal(err)
	}
	const commit = "483edb0165750b273a453b5d5de61183091fda14"
	work := filepath.Join(t.TempDir(), "checkout")
	if err := cache.Checkout(t.Context(), "mirror", commit, work); err != nil {
		t.Fatal(err)
	}
	body, err := fixtureGit(t.Context(), repository, "archive", "--format=tar", commit)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(bytes.NewReader(body))
	files := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeDir {
			continue
		}
		expected, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(work, header.Name)
		if header.Typeflag == tar.TypeSymlink {
			actual, err := os.Readlink(path)
			if err != nil || actual != header.Linkname {
				t.Fatalf("symlink %s: %q %v", header.Name, actual, err)
			}
			continue
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("checkout differs from git archive: %s", header.Name)
		}
		files++
	}
	if files == 0 {
		t.Fatal("checkout compared no files")
	}
}

// Exercise the complete helper protocol on the real multi-pack delta fixture.
// The Git shim changes one decoded blob only AFTER CLI archive succeeds; a
// valid-compression, same-size loose override models the old decoder's bytes.
