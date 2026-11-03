package trustedharness

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func guestBundle(t *testing.T, names []string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, n := range names {
		if err := w.WriteHeader(&tar.Header{Name: n, Mode: 0444, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestGuestBundleClosedAndPinned(t *testing.T) {
	good := guestBundle(t, []string{"kernel", "initramfs"})
	if _, err := GuestFiles(good, digest(good)); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"kernel"}, {"kernel", "kernel", "initramfs"}, {"kernel", "initramfs", "suite-driver"}, {"../kernel", "initramfs"}} {
		b := guestBundle(t, names)
		if _, err := GuestFiles(b, digest(b)); err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
	changed := append([]byte(nil), good...)
	changed[512] ^= 1
	if _, err := GuestFiles(changed, digest(good)); err == nil {
		t.Fatal("changed guest accepted")
	}
	trailing := append(append([]byte(nil), good...), []byte("hidden")...)
	if _, err := GuestFiles(trailing, digest(trailing)); err == nil {
		t.Fatal("hidden trailing guest accepted")
	}
}
func TestSnapshotBindsPathsAndExactPrivateBytes(t *testing.T) {
	root := t.TempDir()
	private := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "candidate"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(private, "input.tar")
	got, err := snapshot(root, output, []string{"candidate"})
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal([]artifactHash{{"candidate", digest([]byte("original")), 8}})
	if got != digest(expected) {
		t.Fatal("artifact identity not bound")
	}
	if err := os.WriteFile(filepath.Join(root, "candidate"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("original")) || bytes.Contains(b, []byte("replacement")) {
		t.Fatal("snapshot changed with producer")
	}
	if err := os.Symlink("candidate", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot(root, filepath.Join(private, "link.tar"), []string{"link"}); err == nil {
		t.Fatal("symlink artifact accepted")
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot(root, filepath.Join(private, "fifo.tar"), []string{"fifo"}); err == nil {
		t.Fatal("FIFO artifact accepted")
	}
	if _, err := snapshot(root, filepath.Join(private, "escape.tar"), []string{"../escape"}); err == nil {
		t.Fatal("escaping artifact accepted")
	}
}
func TestHostWaitpidOwnsVerdict(t *testing.T) {
	dir := t.TempDir()
	driver := filepath.Join(dir, "fixture-driver")
	// Test the process boundary itself; production independently requires an
	// approved static ELF, so this shell fixture can never enter Run.
	if err := os.WriteFile(driver, []byte("#!/bin/sh\nprintf '{\"Outcome\":\"passed\"}'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := runHostCase(context.Background(), driver, dir, "one", "qmp", "guest", time.Now().Add(testtimeout.Start(time.Minute))); err == nil {
		t.Fatal("forged PASS stdout overrode failed host process")
	}
	if err := os.WriteFile(driver, []byte("#!/bin/sh\n[ \"$LD_PRELOAD\" = \"\" ] || exit 1\n[ \"$1\" = --case ] || exit 2\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LD_PRELOAD", "candidate.so")
	if err := runHostCase(context.Background(), driver, dir, "one", "qmp", "guest", time.Now().Add(testtimeout.Start(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runHostCase(ctx, driver, dir, "one", "qmp", "guest", time.Now().Add(testtimeout.Start(time.Minute))); err == nil {
		t.Fatal("timeout passed")
	}
	if time.Since(start) > testtimeout.Start(3*time.Second) {
		t.Fatal("process group did not join after cancellation")
	}
}
func TestFetchRejectsCandidateCredentialEndpointsBeforeNetwork(t *testing.T) {
	for _, url := range []string{"http://example.test/suite", "https://user:secret@example.test/suite", "https://example.test/suite?token=secret", "https://example.test/suite#fragment"} {
		if _, err := Fetch(context.Background(), url, strings.Repeat("a", 64), 1024); err == nil {
			t.Fatalf("accepted %s", url)
		}
	}
}
