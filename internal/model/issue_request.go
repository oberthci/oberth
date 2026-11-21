package model

import (
	"errors"

	"github.com/oberthci/oberth/internal/publicerr"
)

// IssuePatch is the issue_update input. Nil selectors mean omitted, including
// explicit JSON null. A supplied revision must be positive for durable CAS.
// Upstream pointing to empty or global requests an installation-wide issue;
// the service checks authority for that move before changing durable state.
type IssuePatch struct {
	ID                 int64            `json:"id"`
	ExpectedRevision   *int64           `json:"expected_revision,omitempty"`
	ExpectedBodySHA256 string           `json:"expected_body_sha256,omitempty"`
	Title              *string          `json:"title,omitempty"`
	Body               *string          `json:"body,omitempty"`
	WorkStatus         *IssueWorkStatus `json:"work_status,omitempty"`
	Upstream           *string          `json:"upstream,omitempty"`
}

func (v IssuePatch) Clone() IssuePatch {
	v.ExpectedRevision = copyPointer(v.ExpectedRevision)
	v.Title = copyPointer(v.Title)
	v.Body = copyPointer(v.Body)
	v.WorkStatus = copyPointer(v.WorkStatus)
	v.Upstream = copyPointer(v.Upstream)
	return v
}

// Validate reports every independently knowable ID, revision and work-state
// refusal. Body-hash comparison and authorization belong to the durable action.
func (v IssuePatch) Validate() error {
	var errs []error
	if v.ID <= 0 {
		errs = append(errs, publicerr.New(publicerr.InvalidInput, "invalid issue ID", "supply a positive issue ID", nil))
	}
	if v.ExpectedRevision != nil && *v.ExpectedRevision <= 0 {
		errs = append(errs, publicerr.New(publicerr.InvalidInput, "invalid expected revision", "supply a positive expected_revision or omit it", nil))
	}
	if v.WorkStatus != nil && !v.WorkStatus.Valid() {
		errs = append(errs, publicerr.New(publicerr.InvalidInput, "invalid work_status", "use todo, active, waiting or review", nil))
	}
	err := errors.Join(errs...)
	if err == nil {
		return nil
	}
	return publicerr.Classify(err)
}

// IssueFilter is the public issue_list input; scope is supplied by Actor, never
// by a caller-selected list of visible upstreams. State is open, closed or all.
// Empty selectors and zero limits take the service defaults.
type IssueFilter struct {
	Repo             string          `json:"repo,omitempty"`
	WorkStatus       IssueWorkStatus `json:"work_status,omitempty"`
	Kind             IssueKind       `json:"kind,omitempty"`
	State            string          `json:"state,omitempty"`
	Query            string          `json:"query,omitempty"`
	Before           int64           `json:"before,omitempty"`
	Limit            int             `json:"limit,omitempty"`
	StaleAfter       string          `json:"stale_after,omitempty"`
	ActiveStaleAfter string          `json:"active_stale_after,omitempty"`
}
