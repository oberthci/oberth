package main

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func encrypt(ctx context.Context, c config, out io.Writer) (resultErr error) {
	if !cleanPath(c.database) {
		return refused()
	}
	recipient, err := age.ParseX25519Recipient(c.recipient)
	if err != nil {
		return recoveryError(recipientInvalid, err)
	}
	file, err := exclusiveOutput(c.output)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, file.Close(), os.Remove(c.output))
		}
	}()
	report, err := streamEncrypted(ctx, c, file, recipient)
	if err != nil {
		return backupError(err)
	}
	if err := errors.Join(syncOutput(file), syncParent(c.output)); err != nil {
		return err
	}
	if err := encryptedReceipt(ctx, c, report, out); err != nil {
		return err
	}
	success = true
	return nil
}

func streamEncrypted(ctx context.Context, c config, out io.Writer, recipient *age.X25519Recipient) (stateport.BackupReport, error) {
	encrypted, err := age.Encrypt(&boundedWriter{ctx: ctx, writer: out, remaining: math.MaxInt64, progress: c.progress}, recipient)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	backup, err := store.NewBackup(c.database)
	var report stateport.BackupReport
	if err == nil {
		backup.Progress = c.progress
		report, err = backup.Snapshot(ctx, c.options, encrypted)
	}
	return report, errors.Join(err, encrypted.Close())
}

func decrypt(ctx context.Context, c config, out io.Writer) (resultErr error) {
	parent, err := recoveryParent(c)
	if err != nil {
		return err
	}
	identity, err := readIdentity(c.identity)
	if err != nil {
		return err
	}
	if err := regular(c.input, math.MaxInt64, false); err != nil {
		return err
	}
	c.recipient = identity.Recipient().String()
	stage, err := store.NewStage(parent)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, stage.Close()) }()
	plain, err := os.OpenFile(filepath.Join(stage.Path, "authenticated.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, plain.Close()) }()
	if err := authenticate(ctx, c, identity, plain); err != nil {
		return err
	}
	// Only authenticated EOF permits archive parsing or publication.
	if _, err := plain.Seek(0, io.SeekStart); err != nil {
		return err
	}
	report, err := inspectPlain(ctx, c, plain)
	if err != nil {
		return backupError(err)
	}
	return encryptedReceipt(ctx, c, report, out)
}

func authenticate(ctx context.Context, c config, identity *age.X25519Identity, out io.Writer) (resultErr error) {
	file, err := os.Open(c.input)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	plain, err := age.Decrypt(contextReader{ctx: ctx, Reader: file, progress: c.progress}, identity)
	if err != nil {
		return headerError(ctx, file, err)
	}
	limit, _ := c.options.Limit()
	_, err = io.Copy(&boundedWriter{ctx: ctx, writer: out, remaining: limit + (64 << 10), progress: c.progress}, plain)
	return authenticatedStreamError(err)
}

func inspectPlain(ctx context.Context, c config, in io.Reader) (stateport.BackupReport, error) {
	in = contextReader{ctx: ctx, Reader: in, progress: c.progress}
	backup, err := store.NewBackup(c.input)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	if c.mode == "restore" {
		return backup.Restore(ctx, stateport.RestoreRequest{Options: c.options, Destination: c.output}, in)
	}
	return backup.Inspect(ctx, c.options, in)
}

func encryptedReceipt(ctx context.Context, c config, report stateport.BackupReport, out io.Writer) error {
	path := c.output
	if c.mode != "backup" {
		path = c.input
	}
	digest, err := fileDigest(ctx, path, c.progress)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return emitReceipt(ctx, out, receipt{"oberth.encrypted-backup-receipt/v1", report, digest, info.Size(), c.recipient, encryption})
}

func headerError(ctx context.Context, file *os.File, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := age.ExtractHeader(file); err == nil {
		if errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
			return recoveryError(damagedEncrypted, cause)
		}
		return recoveryError(identityMismatch, cause)
	}
	return recoveryError(notBackup, cause)
}
