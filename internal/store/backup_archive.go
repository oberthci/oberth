package store

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/model"
)

// BackupReport contains public verification facts, never database rows.
type BackupReport struct {
	Schema      int             `json:"schema"`
	AuditHead   model.AuditHead `json:"audit_head"`
	Integrity   bool            `json:"integrity"`
	ForeignKeys bool            `json:"foreign_keys"`
}

type backupMember struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type backupManifest struct {
	Format string `json:"format"`
	BackupReport
	Members []backupMember `json:"members"`
}

// WriteVerifiedBackup snapshots one committed SQLite view using the online
// backup API, verifies it, then streams a single database member to output.
// Plaintext staging lives beside the database in a private directory removed
// on return. Callers must provide writable parent storage with room for the
// snapshot plus live WAL growth, and an outer process deadline.
func WriteVerifiedBackup(ctx context.Context, path string, expected int, opts Options, output io.Writer) (BackupReport, error) {
	return writeVerifiedBackup(ctx, path, expected, opts, output, false)
}

// WriteStableVerifiedBackup preserves the isolated maintenance utility's stable
// byte-copy protocol and fixed 128 MiB bound. It is not the live CLI path.
func WriteStableVerifiedBackup(ctx context.Context, path string, expected int, opts Options, output io.Writer) (BackupReport, error) {
	opts.BackupMaxBytes = 128 << 20
	return writeVerifiedBackup(ctx, path, expected, opts, output, true)
}

func writeVerifiedBackup(ctx context.Context, path string, expected int, opts Options, output io.Writer, stable bool) (report BackupReport, resultErr error) {
	if ctx == nil || output == nil || expected < 3 || expected > latestMigrationVersion {
		return report, errors.New("backup arguments refused")
	}
	limit, err := BackupMaxBytes(opts.BackupMaxBytes)
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	output = backupContextWriter{Writer: output, ctx: ctx}
	var snapshot string
	var cleanup func() error
	if stable {
		if _, err := backupMembers(ctx, path, limit); err != nil {
			return report, err
		}
		snapshot, cleanup, err = snapshotCurrentDatabase(path)
	} else {
		snapshot, cleanup, err = snapshotOnlineDatabase(ctx, path, limit)
	}
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	members, err := backupMembers(ctx, snapshot, limit)
	if err != nil {
		return report, err
	}
	report, err = verifyBackupSnapshot(ctx, snapshot, expected, opts)
	if err != nil {
		return BackupReport{}, err
	}
	current, err := backupMembers(ctx, snapshot, limit)
	if err != nil {
		return BackupReport{}, err
	}
	if len(current) != len(members) {
		return BackupReport{}, errors.New("backup snapshot changed")
	}
	for i := range current {
		if current[i] != members[i] {
			return BackupReport{}, errors.New("backup snapshot changed")
		}
	}
	manifest, err := json.Marshal(backupManifest{Format: "oberth.database-backup/v1", BackupReport: report, Members: members})
	if err != nil {
		return BackupReport{}, err
	}
	archive := tar.NewWriter(output)
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return BackupReport{}, err
		}
		if err := archive.WriteHeader(&tar.Header{Name: member.Name, Mode: 0600, Size: member.Size, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return BackupReport{}, err
		}
		f, err := os.Open(filepath.Join(filepath.Dir(snapshot), member.Name))
		if err != nil {
			return BackupReport{}, err
		}
		_, copyErr := io.CopyN(archive, f, member.Size)
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return BackupReport{}, err
		}
	}
	if err := archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return BackupReport{}, err
	}
	if _, err := archive.Write(manifest); err != nil {
		return BackupReport{}, err
	}
	if err := archive.Close(); err != nil {
		return BackupReport{}, err
	}
	return report, ctx.Err()
}

func backupMembers(ctx context.Context, path string, maxBytes int64) ([]backupMember, error) {
	var members []backupMember
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		f, state, err := openSQLiteSourceFile(path+suffix, suffix == "")
		if err != nil {
			return nil, err
		}
		if !state.exists {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if info.Size() < 0 || info.Size() > maxBytes-total {
			_ = f.Close()
			return nil, backupSizeError(total+info.Size(), maxBytes)
		}
		total += info.Size()
		h := sha256.New()
		n, copyErr := io.Copy(h, io.LimitReader(&backupCountingReader{Reader: f, ctx: ctx}, info.Size()+1))
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil || n != info.Size() {
			return nil, errors.New("backup inventory refused")
		}
		members = append(members, backupMember{Name: "oberth.sqlite" + suffix, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("backup main file absent")
	}
	return members, nil
}

func verifyBackupSnapshot(ctx context.Context, snapshot string, expected int, opts Options) (report BackupReport, resultErr error) {
	inspection, err := openCurrent(ctx, snapshot, opts, true)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	version, err := inspection.compatibleSchemaVersion(ctx)
	if err != nil || version != expected {
		return report, errors.New("backup schema refused")
	}
	var integrity string
	if err := inspection.db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&integrity); err != nil || integrity != "ok" {
		return report, errors.New("backup integrity refused")
	}
	rows, err := inspection.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return report, err
	}
	defer func() { _ = rows.Close() }()
	anyRows := rows.Next()
	rowErr := rows.Err()
	closeErr := rows.Close()
	if anyRows || rowErr != nil || closeErr != nil {
		return report, errors.New("backup foreign keys refused")
	}
	head, err := inspection.VerifyAuditState(ctx)
	if err != nil {
		return report, err
	}
	report = BackupReport{Schema: version, AuditHead: head, Integrity: true, ForeignKeys: true}
	// Close SQLite before inventory and archive. Its private SHM is derivative
	// and excluded; legacy stable-copy archives may still contain a WAL.
	if err := inspection.Close(); err != nil {
		return BackupReport{}, err
	}
	return report, ctx.Err()
}

// VerifyBackupArchive validates a fully authenticated plaintext archive in a new
// private directory. It never restores a live database, migrates, or changes
// external audit continuity. The caller must authenticate CMS before calling.
func VerifyBackupArchive(ctx context.Context, input io.Reader, expected int, opts Options) (report BackupReport, resultErr error) {
	if ctx == nil || input == nil || expected < 3 || expected > latestMigrationVersion {
		return report, errors.New("verification arguments refused")
	}
	directory, err := os.MkdirTemp("/tmp", "oberth-backup-verify-")
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(directory)) }()
	limit, err := BackupMaxBytes(opts.BackupMaxBytes)
	if err != nil {
		return report, err
	}
	maxArchiveBytes := limit + (64 << 10)
	bounded := &io.LimitedReader{R: input, N: maxArchiveBytes + 1}
	counter := &backupCountingReader{Reader: bounded, ctx: ctx}
	reader := tar.NewReader(counter)
	expectedBytes := int64(1024)
	var members []backupMember
	var manifestBytes []byte
	var total int64
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return report, err
		}
		if counter.n > maxArchiveBytes {
			return report, backupSizeError(counter.n, maxArchiveBytes)
		}
		expectedBytes += 512 + ((h.Size+511)/512)*512
		if seen[h.Name] || h.Typeflag != tar.TypeReg || h.Mode != 0600 || h.Linkname != "" || len(h.PAXRecords) != 0 || h.Size < 0 {
			return report, errors.New("archive entry refused")
		}
		seen[h.Name] = true
		if h.Name == "manifest.json" {
			if len(manifestBytes) != 0 || h.Size > 8192 {
				return report, errors.New("manifest refused")
			}
			manifestBytes, err = io.ReadAll(io.LimitReader(reader, 8193))
			if err != nil {
				return report, err
			}
			continue
		}
		if len(manifestBytes) != 0 || (h.Name != "oberth.sqlite" && h.Name != "oberth.sqlite-wal") {
			return report, errors.New("archive inventory refused")
		}
		if h.Size > limit-total {
			return report, backupSizeError(total+h.Size, limit)
		}
		total += h.Size
		if len(members) == 0 && h.Name != "oberth.sqlite" {
			return report, errors.New("archive order refused")
		}
		destination := filepath.Join(directory, "oberth.sqlite")
		if h.Name == "oberth.sqlite-wal" {
			destination += "-wal"
		}
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- exact two fixed names in fresh private directory.
		if err != nil {
			return report, err
		}
		hash := sha256.New()
		n, copyErr := io.CopyN(io.MultiWriter(file, hash), reader, h.Size)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return report, err
		}
		members = append(members, backupMember{Name: h.Name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	// The canonical writer emits exactly the tar trailer; trailing data is not
	// another accepted archive or a place for an unaudited extra member.
	var trailing [1]byte
	n, err := counter.Read(trailing[:])
	if n != 0 || !errors.Is(err, io.EOF) || counter.n != expectedBytes || counter.n > maxArchiveBytes {
		return report, errors.New("archive trailing bytes refused")
	}
	if len(members) == 0 || len(manifestBytes) == 0 {
		return report, errors.New("archive incomplete")
	}
	var manifest backupManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return report, err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, manifestBytes) || manifest.Format != "oberth.database-backup/v1" || len(manifest.Members) != len(members) {
		return report, errors.New("manifest canonical shape refused")
	}
	for i := range members {
		if manifest.Members[i] != members[i] {
			return report, errors.New("manifest inventory refused")
		}
	}
	report, err = verifyBackupSnapshot(ctx, filepath.Join(directory, "oberth.sqlite"), expected, opts)
	if err != nil {
		return BackupReport{}, err
	}
	actual, err := json.Marshal(report)
	if err != nil {
		return BackupReport{}, err
	}
	claimed, err := json.Marshal(manifest.BackupReport)
	if err != nil || !bytes.Equal(actual, claimed) {
		return BackupReport{}, errors.New("manifest verification mismatch")
	}
	return report, ctx.Err()
}

type backupCountingReader struct {
	io.Reader
	ctx context.Context
	n   int64
}

func (r *backupCountingReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, e := r.Reader.Read(p)
	r.n += int64(n)
	return n, e
}

type backupContextWriter struct {
	io.Writer
	ctx context.Context
}

func (w backupContextWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(b)
}
