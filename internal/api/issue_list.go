package api

import (
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// IssueListPage preserves the dashboard's field names while keeping full body,
// body text and internal scope/sequence metadata out of polling lists.
// The authenticated issue detail route serves the body.
type IssueListPage struct {
	Issues     []IssueListSummary
	NextBefore int64
	Total      int64             `json:"total"`
	Counts     model.IssueCounts `json:"counts"`
}
type IssueListSummary struct {
	Revision    int64 `json:"revision"`
	ID          int64
	RepoID      int64
	Kind        model.IssueKind
	Branch      string
	Title       string
	State       model.IssueState
	Occurrences int64
	CIOrigin    string
	CIWorkID    string
	Upstream    string           `json:"upstream"`
	Lock        *model.IssueLock `json:"lock"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
}
