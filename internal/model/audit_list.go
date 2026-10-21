package model

import (
	"errors"
	"time"
)

// ErrBackupRecordUnreadable marks a backup.completed audit row whose details
// do not decode. The database answered; the record itself is the problem.
var ErrBackupRecordUnreadable = errors.New("backup audit record is unreadable")

// BackupStatus is the last completed backup recorded in the audit chain.
type BackupStatus struct {
	At            string          `json:"at"`
	Head          BackupAuditHead `json:"head"`
	ArchiveSHA256 string          `json:"archive_sha256"`
}

type BackupAuditHead struct {
	ID     int64  `json:"id"`
	SHA256 string `json:"sha256"`
}

// AuditListFilter selects audit actions for audit_list. Zero values mean no
// constraint. Before is an exclusive audit-ID cursor; results are newest
// first.
type AuditListFilter struct {
	ActionPrefix string
	Actor        string
	// RepoID and RepoName together select actions about one repository:
	// repository-typed rows, details naming the repository, and runs and
	// promotions that belong to it. Details may name it bare or qualified
	// (upstream/org/repo); RepoQualifiedName carries the latter when known.
	RepoID            int64
	RepoName          string
	RepoQualifiedName string
	Since             time.Time
	Until             time.Time
	Before            int64
	Limit             int
}
