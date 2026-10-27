package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminReadPublicFile(t *testing.T) {
	t.Parallel()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificate := filepath.Join(directory, "tls.crt")
	body := []byte("public certificate\n")
	if err := os.WriteFile(certificate, body, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(directory, "ca.crt")
	allowed := map[string]bool{certificate: true, missing: true}
	var output bytes.Buffer
	if err := adminReadPublicFiles(t.Context(), []string{missing, certificate}, &output, allowed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), body) {
		t.Fatal("certificate bytes were changed")
	}
	for _, args := range [][]string{{"read-file", directory}, {"read-file", "relative"}, {"read-file", missing}, {"hash-db", "--database", "/proc/1/environ"}} {
		if err := runAdmin(t.Context(), args, io.Discard); err == nil {
			t.Fatalf("invalid file invocation accepted: %v", args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := adminReadPublicFiles(ctx, []string{certificate}, io.Discard, allowed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	sentinel := errors.New("output closed")
	if err := adminReadPublicFiles(t.Context(), []string{certificate}, adminFailWriter{sentinel}, allowed); !errors.Is(err, sentinel) {
		t.Fatalf("copy write error lost: %v", err)
	}
}

type adminFailWriter struct{ err error }

func (w adminFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAdminPublicFileAllowlistAndProjection(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/etc/oberth/tls/tls.key", "/run/oberth-identities/server/ssh_host_key", "/run/oberth-identities/upstream/id_ed25519", "/var/run/secrets/kubernetes.io/serviceaccount/token", "/proc/1/environ", "/etc/oberth/tls/../tls/tls.crt"} {
		if err := runAdmin(t.Context(), []string{"read-file", path}, io.Discard); err == nil {
			t.Fatalf("private/nonexact path accepted: %s", path)
		}
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projected := filepath.Join(directory, "..2026")
	if err := os.Mkdir(projected, 0o700); err != nil {
		t.Fatal(err)
	}
	certificate := filepath.Join(projected, "tls.crt")
	if err := os.WriteFile(certificate, []byte("public certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026", filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "tls.crt")
	if err := os.Symlink("..data/tls.crt", path); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{path: true}
	var output bytes.Buffer
	if err := adminReadPublicFiles(t.Context(), []string{path}, &output, allowed); err != nil || output.String() != "public certificate" {
		t.Fatalf("projected certificate: %q %v", output.String(), err)
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "tls.crt"), filepath.Join(projected, "tls.key")} {
		if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		output.Reset()
		if err := adminReadPublicFiles(t.Context(), []string{path}, &output, allowed); err == nil || output.Len() != 0 {
			t.Fatalf("symlink escaped public allowlist: %v", err)
		}
	}
	// An untrusted fallback must be checked even if the first file exists.
	if err := adminReadPublicFiles(t.Context(), []string{certificate, "/proc/1/environ"}, io.Discard, map[string]bool{certificate: true}); err == nil {
		t.Fatal("unchecked fallback")
	}
}

// The wrapper swaps an entry after Lstat and immediately before OpenFile. This
// exercises the race deterministically without relying on O_NOFOLLOW behavior.
type swappingAdminRoot struct {
	*os.Root
	beforeOpen func()
	opened     *os.File
}

func (r *swappingAdminRoot) OpenFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	r.beforeOpen()
	file, err := r.Root.OpenFile(name, flags, mode)
	r.opened = file
	return file, err
}

func TestAdminPublicFileSwapRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"symlink before lstat", "symlink after lstat", "regular replacement after lstat"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			certificate := filepath.Join(directory, "tls.crt")
			key := filepath.Join(directory, "tls.key")
			if err := os.WriteFile(certificate, []byte("public certificate"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(key, []byte("private key canary"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			swap := func() {
				if err := os.Rename(certificate, filepath.Join(directory, "original.crt")); err != nil {
					t.Fatal(err)
				}
				if kind == "regular replacement after lstat" {
					if err := os.Rename(key, certificate); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("tls.key", certificate); err != nil {
					t.Fatal(err)
				}
			}
			checked := &swappingAdminRoot{Root: root, beforeOpen: swap}
			if kind == "symlink before lstat" {
				swap()
				checked.beforeOpen = func() { t.Fatal("symlink entry reached open") }
			}
			file, err := openRegularPublicAdminFile(checked, "tls.crt")
			if err == nil || file != nil {
				if file != nil {
					_ = file.Close()
				}
				t.Fatalf("swapped file accepted: %v", err)
			}
			if checked.opened != nil {
				if _, err := checked.opened.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("refused file descriptor was not closed: %v", err)
				}
			}
		})
	}
}
