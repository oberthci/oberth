package installer

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupCommandKeepsStderrOutOfArchive(t *testing.T) {
	var out bytes.Buffer
	if err := defaultRunCommandIO(t.Context(), nil, &out, "sh", "-c", "printf archive; printf diagnostic >&2"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "archive" {
		t.Fatalf("binary stdout contaminated: %q", out.String())
	}
	out.Reset()
	err := defaultRunCommandIO(t.Context(), nil, &out, "sh", "-c", "printf untrusted-output; printf failure-diagnostic >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "failure-diagnostic") || strings.Contains(err.Error(), "untrusted-output") {
		t.Fatal(err)
	}
}

func TestBackupArchiveLimitRefusesBeforeBody(t *testing.T) {
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	if err := w.WriteHeader(&tar.Header{Name: backupManifestName, Mode: 0600, Typeflag: tar.TypeReg, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("{}"))
	// Mock a large member: rejection must happen before trying to read it.
	const limit int64 = 200 << 20
	size := limit + (64 << 10) + 1
	if err := w.WriteHeader(&tar.Header{Name: backupMemberDatabase, Mode: 0600, Typeflag: tar.TypeReg, Size: size}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "oversized.cms")
	if err := os.WriteFile(file, append(fakeEnvelopeHeader(), raw.Bytes()...), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := openBackupArchive(t.Context(), file, fakeEnvelopeCipher{}, t.TempDir(), limit)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(size)) || !strings.Contains(err.Error(), fmt.Sprint(size-1)) || !strings.Contains(err.Error(), "--max-database-bytes") {
		t.Fatal(err)
	}
}
