package model

import "time"

// AuditListFilter selects audit actions for audit_list. Zero values mean no
// constraint. Before is an exclusive audit-ID cursor; results are newest
// first.
type AuditListFilter struct {
	ActionPrefix string
	Actor        string
	// RepoID and RepoName together select actions about one repository:
	// repository-typed rows, details naming the repository, and runs and
	// promotions that belong to it.
	RepoID   int64
	RepoName string
	Since    time.Time
	Until    time.Time
	Before   int64
	Limit    int
}
