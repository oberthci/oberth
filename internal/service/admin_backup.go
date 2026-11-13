package service

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

// PrepareAdminBackup verifies the snapshot and persists the audit outcome before
// HTTP headers. A failed audit closes the snapshot without exposing its bytes.
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
	if !s.adminBackupLock.TryLock() {
		return nil, api.ErrAdminBackupBusy
	}
	archive, err := s.administration.DatabaseBackup(ctx, limit)
	if err != nil {
		s.adminBackupLock.Unlock()
		return nil, err
	}
	return &lockedAdminBackup{ReadCloser: archive, lock: s.adminBackupLock}, nil
}

type lockedAdminBackup struct {
	io.ReadCloser
	lock *sync.Mutex
	once sync.Once
	err  error
}

func (b *lockedAdminBackup) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close(); b.lock.Unlock() })
	return b.err
}
