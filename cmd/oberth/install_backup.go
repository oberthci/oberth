package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (s installationSession) backup(ctx context.Context, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	source, err := s.snapshotSource()
	if err != nil {
		return err
	}
	defer source.remote.Close()
	archive, err := installer.Backup(ctx, installer.BackupConfig{Source: source, Options: s.maintenance(source).Options, Recipient: s.profile.BackupRecipient, Path: s.request.text("out"), DryRun: s.request.enabled("dry-run")})
	return s.backupResult(out, archive, err)
}

func (s installationSession) backupResult(out io.Writer, archive installer.Archive, err error) error {
	var warning *publicerr.Error
	if err != nil {
		var problem *installer.Problem
		if !errors.As(err, &problem) || (problem.Code != "backup-directory-sync" && problem.Code != "backup-cleanup") {
			return err
		}
		warning = publicerr.Classify(installationFailure(err))
		s.app.failed(warning)
	}
	if s.request.text("output") == "json" {
		return json.NewEncoder(out).Encode(struct {
			installer.Archive
			Warning *publicerr.Error `json:"warning,omitempty"`
		}{archive, warning})
	}
	if archive.DryRun {
		_, err := fmt.Fprintln(out, "Planned backup: "+archive.Path)
		return err
	}
	if _, err := fmt.Fprintln(out, "Backup: "+archive.Path); err != nil {
		return err
	}
	for _, line := range []string{"SHA256: " + archive.SHA256, fmt.Sprintf("Database schema: %d", archive.Report.Schema), archive.StoreSnapshotHint} {
		if err = writeWrapped(out, line); err != nil {
			return err
		}
	}
	return nil
}
