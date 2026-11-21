package service

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"io"
)

// Backup holds the single slot through the sink's close. The configured main
// Snapshotter owns the archive format; the service does not invent encryption.
func (s *Service) Backup(ctx context.Context, a actor.Actor, out io.WriteCloser) (report stateport.BackupReport, err error) {
	a, err = s.peopleAllow(ctx, a, "backup")
	if err != nil {
		return report, err
	}
	if out == nil {
		return report, bad("backup_sink")
	}
	if !s.backup.TryLock() {
		return report, refuse(backupRunning)
	}
	defer s.backup.Unlock()
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, out.Close())
		}
	}()
	if err = s.c.People.BackupOptions.Validate(); err != nil {
		return report, err
	}
	if err = s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(a, "administration", "", ""), model.AuditAction{Actor: a.Identity, Action: "backup.started", ResourceType: "installation"}); err != nil {
		return report, err
	}
	report, err = s.c.People.Backup.Snapshot(ctx, s.c.People.BackupOptions, out)
	if err != nil {
		return report, err
	}
	closed = true
	if err = out.Close(); err != nil {
		return report, err
	}
	err = s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(a, "administration", "", ""), model.AuditAction{Actor: a.Identity, Action: "backup.completed", ResourceType: "installation"})
	return report, err
}
