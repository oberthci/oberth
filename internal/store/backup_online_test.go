package store

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exercise the >128 MiB regression with real SQLite pages and a disk writer,
// never a database-sized Go buffer. Dropping the padding table restores the
// ordinary schema while leaving a realistic large freelist in the main file.
func TestOnlineBackupLargeRestoreRoundTrip(t *testing.T) {
	path, db := backupFixture(t)
	if _, err := db.db.Exec("CREATE TABLE backup_padding(data BLOB); INSERT INTO backup_padding VALUES(zeroblob(135266304)); DROP TABLE backup_padding"); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "database.tar")
	out, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	report, err := WriteVerifiedBackup(t.Context(), path, 20, Options{}, out)
	if closeErr := out.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	info, err := os.Stat(archivePath)
	if err != nil || info.Size() <= 128<<20 {
		t.Fatalf("fixture must cross old cap: %v %v", info, err)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyBackupArchive(t.Context(), f, 20, Options{})
	_ = f.Close()
	if err != nil || !reflect.DeepEqual(report, verified) {
		t.Fatalf("archive verification: %+v %v", verified, err)
	}
	restored := restoreOnlineArchive(t, archivePath)
	inspection, err := InspectCurrent(t.Context(), restored, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inspection.Close() }()
	version, err := inspection.compatibleSchemaVersion(t.Context())
	if err != nil || version != 20 {
		t.Fatalf("restore changed schema: %d %v", version, err)
	}
	head, err := inspection.VerifyAuditState(t.Context())
	if err != nil || !reflect.DeepEqual(head, report.AuditHead) {
		t.Fatalf("restore lost committed audit: %+v %v", head, err)
	}
}

func restoreOnlineArchive(t *testing.T, archivePath string) string {
	t.Helper()
	in, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	destination := filepath.Join(t.TempDir(), "oberth.sqlite")
	r := tar.NewReader(in)
	seen := map[string]bool{}
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = true
		if h.Name == "manifest.json" {
			continue
		}
		if h.Name != "oberth.sqlite" {
			t.Fatalf("online snapshot unexpectedly carried %s", h.Name)
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.CopyN(file, r, h.Size)
		if closeErr := file.Close(); err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
	}
	if len(seen) != 2 || !seen["oberth.sqlite"] || !seen["manifest.json"] {
		t.Fatalf("unexpected inventory: %v", seen)
	}
	return destination
}

func TestOnlineBackupLimitCountsLogicalPagesNotWAL(t *testing.T) {
	path, db := backupFixture(t)
	var pages, pageSize int64
	if err := db.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	limit := pages * pageSize
	var rawSize int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		rawSize += info.Size()
	}
	if rawSize <= limit {
		t.Fatalf("fixture requires duplicate WAL versions: %d <= %d", rawSize, limit)
	}
	var output bytes.Buffer
	opts := Options{BackupMaxBytes: limit}
	report, err := WriteVerifiedBackup(t.Context(), path, 20, opts, &output)
	if err != nil {
		t.Fatalf("logical %d, main+WAL %d: %v", limit, rawSize, err)
	}
	verified, err := VerifyBackupArchive(t.Context(), bytes.NewReader(output.Bytes()), 20, opts)
	if err != nil || !reflect.DeepEqual(report, verified) {
		t.Fatal(verified, err)
	}
	if _, err := VerifyBackupArchive(t.Context(), bytes.NewReader(output.Bytes()), 20, Options{BackupMaxBytes: limit - 1}); err == nil || !strings.Contains(err.Error(), fmt.Sprint(limit)) || !strings.Contains(err.Error(), "--max-database-bytes") {
		t.Fatalf("restore verifier ignored configured limit: %v", err)
	}
	output.Reset()
	_, err = WriteVerifiedBackup(t.Context(), path, 20, Options{BackupMaxBytes: limit - 1}, &output)
	if err == nil || output.Len() != 0 || !strings.Contains(err.Error(), fmt.Sprint(limit)) || !strings.Contains(err.Error(), fmt.Sprint(limit-1)) {
		t.Fatalf("size refusal must name actual bytes and limit, with no output: %v", err)
	}
	if _, err := VerifyBackupArchive(t.Context(), bytes.NewReader(output.Bytes()), 20, opts); err == nil {
		t.Fatal("empty archive accepted")
	}
}

func TestBackupConfiguredLimitBounds(t *testing.T) {
	for _, value := range []int64{-1, HardBackupMaxBytes + 1} {
		if _, err := BackupMaxBytes(value); err == nil {
			t.Fatalf("invalid bound %d accepted", value)
		}
	}
	for _, value := range []int64{0, 1, DefaultBackupMaxBytes, HardBackupMaxBytes} {
		got, err := BackupMaxBytes(value)
		if value == 0 {
			value = DefaultBackupMaxBytes
		}
		if err != nil || got != value {
			t.Fatal(got, err)
		}
	}
}

func TestStableMaintenanceBackupRemainsLegacyCompatible(t *testing.T) {
	path, _ := backupFixture(t)
	var output bytes.Buffer
	report, err := WriteStableVerifiedBackup(context.Background(), path, 20, Options{}, &output)
	if err != nil {
		t.Fatal(err)
	}
	seenWAL := false
	reader := tar.NewReader(bytes.NewReader(output.Bytes()))
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seenWAL = seenWAL || h.Name == "oberth.sqlite-wal"
	}
	if !seenWAL {
		t.Fatal("stable maintenance protocol must still retain committed WAL")
	}
	verified, err := VerifyBackupArchive(t.Context(), bytes.NewReader(output.Bytes()), 20, Options{BackupMaxBytes: 128 << 20})
	if err != nil || !reflect.DeepEqual(report, verified) {
		t.Fatal(verified, err)
	}
}
