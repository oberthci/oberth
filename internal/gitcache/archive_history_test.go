package gitcache_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/archivecheck"
	"github.com/oberthci/oberth/internal/gitcache"
)

func TestShadowFixtureProcess(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "shadow-fixture" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	if len(args) != 4 {
		os.Exit(2)
	}
	os.Exit(gitcache.RunShadowHelper(context.Background(), args, os.Stdout))
}

func TestShadowRealCorruptionFixture(t *testing.T) {
	repository := extractHistoryFixture(t)
	root := t.TempDir()
	// Preserve the exact incident-triggering pack/delta layout, not a repack.
	if err := os.Symlink(repository, filepath.Join(root, "mirror.git")); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := gitcache.New(gitcache.Config{Root: root, Backend: gitcache.BackendGoGit, Shadow: true, ShadowContext: t.Context(), ShadowCommand: []string{executable, "-test.run=^TestShadowFixtureProcess$", "--", "shadow-fixture"}, Upstream: func(string) (string, error) { return "", errors.New("shadow must not fetch") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.CloseShadow)
	const commit = "483edb0165750b273a453b5d5de61183091fda14"
	var actual bytes.Buffer
	if err := cache.ArchiveTar(t.Context(), "mirror", commit, &actual); err != nil {
		t.Fatal(err)
	}
	expected, err := archivecheck.Git(t.Context(), repository, "archive", "--format=tar", commit)
	if err != nil || !bytes.Equal(actual.Bytes(), expected) {
		t.Fatalf("shadow changed CLI bytes: %v", err)
	}
	if err := cache.Checkout(t.Context(), "mirror", commit, filepath.Join(t.TempDir(), "work")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := cache.WaitShadow(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := cache.ShadowStats(); stats.Runs != 2 || stats.Errors != 0 || stats.Mismatches != 0 || stats.Skipped != 0 {
		t.Fatalf("incident fixture parity: %+v", stats)
	}
}

func TestArchiveRealHistory(t *testing.T) {
	repository := os.Getenv("OBERTH_ARCHIVE_PARITY_REPO")
	if repository == "" {
		t.Skip("set OBERTH_ARCHIVE_PARITY_REPO to run every commit of a real multi-pack mirror")
	}
	if repository == "fixture" {
		repository = extractHistoryFixture(t)
	}
	checker, err := archivecheck.New(repository)
	if err != nil {
		t.Fatal(err)
	}
	defer checker.Close()
	body, err := archivecheck.Git(t.Context(), repository, "rev-list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	commits := strings.Fields(string(body))
	if len(commits) == 0 {
		t.Fatal("empty history")
	}
	for i, commit := range commits {
		if err := checker.Check(t.Context(), commit); err != nil {
			t.Fatalf("commit=%s: %v", commit, err)
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
	if _, err := archivecheck.Git(t.Context(), directory, "fsck", "--full"); err != nil {
		t.Fatal(err)
	}
	count, err := archivecheck.Git(t.Context(), directory, "rev-list", "--all", "--count")
	if err != nil || strings.TrimSpace(string(count)) != "878" {
		t.Fatalf("fixture history changed: %s %v", count, err)
	}
	return directory
}

// Production seeds the checked-out workspace, so archive-only parity is not
// enough. This real commit also corrupts with the released streaming decoder.
func TestCheckoutRealDeltaParity(t *testing.T) {
	repository := extractHistoryFixture(t)
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: gitcache.BackendGoGit, Upstream: func(string) (string, error) { return repository, nil }})
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
	body, err := archivecheck.Git(t.Context(), repository, "archive", "--format=tar", commit)
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
func TestShadowHelperCorruptedRealFixtureHashGuard(t *testing.T) {
	for _, attributes := range []bool{false, true} {
		t.Run(fmt.Sprintf("export-rules=%v", attributes), func(t *testing.T) {
			repository := extractHistoryFixture(t)
			// Use an unpacked copy for deterministic byte corruption. The original
			// packed/delta layout is exercised by TestShadowRealCorruptionFixture.
			packs := filepath.Join(repository, "objects", "pack")
			outside := filepath.Join(t.TempDir(), "packs")
			if err := os.Rename(packs, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(packs, 0700); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(outside, "*.pack"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				input, err := os.Open(file)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(t.Context(), "git", "-C", repository, "unpack-objects", "-r")
				cmd.Stdin = input
				output, err := cmd.CombinedOutput()
				_ = input.Close()
				if err != nil {
					t.Fatalf("unpack fixture: %v %s", err, output)
				}
			}
			const commit = "483edb0165750b273a453b5d5de61183091fda14"
			const name = "types/types.go"
			if attributes {
				if err := os.MkdirAll(filepath.Join(repository, "info"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repository, "info", "attributes"), []byte(name+" export-ignore\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			entry, err := archivecheck.Git(t.Context(), repository, "ls-tree", commit, name)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(string(entry))
			if len(fields) != 4 {
				t.Fatalf("fixture entry: %q", entry)
			}
			hash := fields[2]
			blob, err := exec.CommandContext(t.Context(), "git", "-C", repository, "cat-file", "blob", hash).Output()
			if err != nil || len(blob) == 0 {
				t.Fatalf("fixture blob: %v", err)
			}
			blob[len(blob)/2] ^= 1
			var compressed bytes.Buffer
			z := zlib.NewWriter(&compressed)
			if _, err := fmt.Fprintf(z, "blob %d\x00", len(blob)); err != nil {
				t.Fatal(err)
			}
			if _, err := z.Write(blob); err != nil {
				t.Fatal(err)
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			corrupt := filepath.Join(root, "corrupt")
			if err := os.WriteFile(corrupt, compressed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			id := hash
			target := filepath.Join(repository, "objects", id[:2], id[2:])
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0600); err != nil {
				t.Fatal(err)
			}
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			git, err = filepath.Abs(git)
			if err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			shim := filepath.Join(root, "git")
			script := "#!/bin/sh\nset -eu\n" +
				"test \"${GIT_ATTR_NOSYSTEM-}\" = 1\ntest \"$HOME\" = /dev/null\ntest \"${XDG_CONFIG_HOME-}\" = /dev/null\n" +
				"test \"$1\" = -c\ntest \"$2\" = core.attributesFile=/dev/null\n" + quote(git) + " \"$@\"\ncase \"$*\" in *--format=tar*) cp " + quote(corrupt) + " " + quote(target) + " ;; esac\n"
			if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestShadowFixtureProcess$", "--", "shadow-fixture", repository, commit, shim, "/dev/null")
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 21 || len(output) != 0 {
				t.Fatalf("RunShadowHelper must return gogit_hash_guard (21), not an attr gap: %v output=%q", err, output)
			}
		})
	}
}
