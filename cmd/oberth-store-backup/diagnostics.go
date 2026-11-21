package main

import (
	"errors"
	"os"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/store"
)

func backupError(err error) error {
	var size *store.SizeLimitError
	if errors.As(err, &size) {
		result := recoveryError(configuredTooLarge, err)
		result.Detail = size.Error()
		return result
	}
	var mismatch *store.ProtocolMismatchError
	if errors.As(err, &mismatch) {
		return recoveryError(foreignInstallation, err)
	}
	return err
}

func recoveryDiagnostic(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return recoveryError(volumeFull, err)
	}
	var public *publicerr.Error
	if errors.As(err, &public) {
		return public
	}
	if errors.Is(err, os.ErrNotExist) {
		return recoveryError(fileMissing, err)
	}
	if errors.Is(err, os.ErrExist) {
		return recoveryError(fileExists, err)
	}
	if errors.Is(err, os.ErrPermission) {
		return recoveryError(accessDenied, err)
	}
	return recoveryError(recoveryIO, err)
}

type recoveryRefusal int

const (
	bindingInvalid recoveryRefusal = iota
	argumentsInvalid
	identityPublic
	identityInvalid
	identityProtected
	identityMismatch
	recipientInvalid
	outputPublic
	notBackup
	damagedEncrypted
	archiveTooLarge
	configuredTooLarge
	foreignInstallation
	idleStalled
	volumeFull
	fileMissing
	fileExists
	accessDenied
	recoveryIO
)

func recoveryError(kind recoveryRefusal, cause error) *publicerr.Error {
	reason, remedy := recoveryMessage(kind)
	code := publicerr.InvalidInput
	if kind >= volumeFull || kind == idleStalled {
		code = publicerr.Unavailable
	}
	if kind == foreignInstallation {
		code = publicerr.SchemaRefused
	}
	if kind == fileExists {
		code = publicerr.Conflict
	}
	return publicerr.New(code, reason, remedy, cause)
}

func recoveryMessage(kind recoveryRefusal) (string, string) {
	switch kind {
	case bindingInvalid:
		return "backup paths or protocol bindings are invalid", "Use absolute local paths and the exact installation schema identity and audit domain."
	case argumentsInvalid:
		return "backup command arguments are invalid", "Use the documented backup flags and exact installation bindings."
	case identityPublic:
		return "identity file permissions allow group or other access", "Set the selected identity file permissions to 0400 or 0600."
	case identityInvalid:
		return "backup identity file is invalid", "Select a file containing one age X25519 identity, with optional comments."
	case identityProtected:
		return "this identity is passphrase-protected", "Use an unprotected identity file readable only by its owner."
	case identityMismatch:
		return "not a backup for this key", "Check the selected identity and use an intact backup encrypted for it."
	case recipientInvalid:
		return "backup recipient is invalid", "Supply one age X25519 public recipient using --recipient."
	case outputPublic:
		return "backup output directory is not private", "Select an output directory accessible only to its owner."
	}
	return recoveryOperationMessage(kind)
}

func recoveryOperationMessage(kind recoveryRefusal) (string, string) {
	switch kind {
	case notBackup:
		return "input is not an Oberth age backup", "Select an intact age-encrypted Oberth backup."
	case damagedEncrypted:
		return "encrypted backup is truncated or altered", "Select an intact age backup and retry with its matching identity."
	case archiveTooLarge:
		return "decrypted archive exceeds the configured bound", "Check the archive or raise --max-database-bytes within the 4 GiB database limit."
	case configuredTooLarge:
		return "database exceeds the configured backup limit", "Raise --max-database-bytes within the 4 GiB limit, or use a smaller database."
	case foreignInstallation:
		return "this backup belongs to another installation", "Select a backup matching this installation's schema identity and audit domain."
	case idleStalled:
		return "backup or restore made no progress for 5 minutes", "Check the source, destination and storage, then retry."
	case volumeFull:
		return "backup or restore volume is full", "Free disk space or quota on the database and staging volumes, then retry."
	case fileMissing:
		return "selected backup file does not exist", "Select an existing database, archive or identity file as required by the command."
	case fileExists:
		return "backup output already exists", "Select an absent output path; restore never replaces a database."
	case accessDenied:
		return "backup file access was denied", "Check access permissions for the selected files and their parent directories."
	case recoveryIO:
		return "backup I/O failed", "Check the selected storage and retry the backup operation."
	}
	panic("unknown recovery refusal")
}
