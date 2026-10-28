package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func TestBackupStreamingSizeRefusalCancelsAndRemovesPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deps := backupDeps{runIO: func(ctx context.Context, _ io.Reader, out io.Writer, _ string, _ ...string) error {
		_, err := out.Write([]byte("01234567890"))
		<-ctx.Done() // The size guard, not this liveness bound, must cancel it.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("size refusal did not cancel the producer")
		}
		return err
	}}
	path := filepath.Join(t.TempDir(), "partial")
	_, err := streamBackupFile(ctx, deps, path, 10, nil)
	if err == nil || !strings.Contains(err.Error(), "11 bytes exceeds limit 10 bytes") {
		t.Fatalf("size error lacks facts: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial file retained", err)
	}
}

func TestBackupStreamingCancellationAndFailedExit(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			deps := backupDeps{runIO: func(ctx context.Context, _ io.Reader, out io.Writer, _ string, _ ...string) error {
				_, _ = out.Write([]byte("partial"))
				if canceled {
					return ctx.Err()
				}
				return errors.New("fixture exit 1")
			}}
			path := filepath.Join(t.TempDir(), "archive")
			_, err := streamBackupFile(ctx, deps, path, 100, nil)
			if err == nil {
				t.Fatal("failed stream accepted")
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial retained", err)
			}
		})
	}
}

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

func TestBackupOldLimitFlagFallsBackToStableCopy(t *testing.T) {
	archive, raw, _ := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat)
	source.database, source.rawFiles = archive, raw
	var output bytes.Buffer
	deps := source.deps(&output)
	run := deps.runIO
	deps.runIO = func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "backup database") {
			return errors.New("oberth: flag provided but not defined: -max-database-bytes")
		}
		return run(ctx, in, out, name, args...)
	}
	path := filepath.Join(t.TempDir(), "database.tar")
	mode, err := snapshotServerDatabase(t.Context(), deps, DefaultNamespace, "oberth-1", store.LatestMigrationVersion(), store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}, t.TempDir(), path)
	if err != nil || mode != snapshotStableCopy {
		t.Fatal(mode, err)
	}
	if _, err := verifyDatabaseArchive(t.Context(), path, store.LatestMigrationVersion(), store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}, 0); err != nil {
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
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(size)) || !strings.Contains(err.Error(), fmt.Sprint(size-1)) {
		t.Fatal(err)
	}
}

func TestBackupCompatibilityHashChecksSizeBeforeHashing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pinned Linux utility stat syntax")
	}
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("01234567890"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", utilitySQLiteHashesScript, "sh", path, "10")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "11 bytes exceeds limit 10 bytes") {
		t.Fatalf("compatibility guard failed: %s %v", out, err)
	}
}
