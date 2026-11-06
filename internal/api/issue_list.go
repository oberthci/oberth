package api

import (
	"encoding/json"
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
	WorkStatus  model.IssueWorkStatus `json:"work_status"`
	Stale       bool                  `json:"stale"`
	Revision    int64                 `json:"revision"`
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

// Closed list entries have no active work state; detail records keep their history.
func (issue IssueListSummary) MarshalJSON() ([]byte, error) {
	type wire IssueListSummary
	var work *model.IssueWorkStatus
	var stale *bool
	if issue.State != model.IssueClosed {
		work = &issue.WorkStatus
		stale = &issue.Stale
	}
	return json.Marshal(struct {
		wire
		WorkStatus *model.IssueWorkStatus `json:"work_status,omitempty"`
		Stale      *bool                  `json:"stale,omitempty"`
	}{wire: wire(issue), WorkStatus: work, Stale: stale})
}
