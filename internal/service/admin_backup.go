package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

// adminBackupFile removes its private staging file when the response closes.
type adminBackupFile struct{ *os.File }

func (f *adminBackupFile) Close() error { return errors.Join(f.File.Close(), os.Remove(f.Name())) }

// PrepareAdminBackup stages the complete verified archive before sending HTTP
// headers. Failed audit persistence or an interrupted snapshot exposes no bytes.
func (s *API) PrepareAdminBackup(ctx context.Context, actor api.Actor, raw json.RawMessage) (io.ReadCloser, error) {
	value, err := s.administrationTool(ctx, actor, "admin_database_backup", raw)
	if err != nil {
		return nil, err
	}
	return value.(io.ReadCloser), nil
}

func (s *API) stageAdminBackup(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MaxDatabaseBytes int64 `json:"max_database_bytes"`
	}
	if err := decodeAdministration(raw, &args); err != nil {
		return nil, err
	}
	limit, err := store.BackupMaxBytes(args.MaxDatabaseBytes)
	if err != nil {
		return nil, refuse(ErrInvalidInput, "invalid database backup size bound")
	}
	if s.administration.DatabaseBackup == nil {
		return nil, refuse(ErrUnavailable, "verified database backup")
	}
	file, err := os.CreateTemp("", "oberth-admin-backup-")
	if err != nil {
		return nil, err
	}
	archive := &adminBackupFile{file}
	keep := false
	defer func() {
		if !keep {
			_ = archive.Close()
		}
	}()
	if err := s.administration.DatabaseBackup(ctx, &adminBackupWriter{writer: file, remaining: limit + (64 << 10)}, limit); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	keep = true
	return archive, nil
}

type adminBackupWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *adminBackupWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, refuse(ErrInvalidInput, "database backup archive exceeds size bound")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
