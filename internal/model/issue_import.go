package model

import (
	"slices"
	"time"
)

// IssueHistory is imported text evidence, never an authority or local audit action.
// Empty Text means the source retained no separate text for that entry.
type IssueHistory struct {
	Kind   string     `json:"kind"`
	At     time.Time  `json:"at"`
	Author string     `json:"author"`
	Text   string     `json:"text,omitempty"`
	State  IssueState `json:"state,omitempty"`
}

// IssueImportSource identifies the immutable imported record set after local edits.
// Digests describe content; they never identify an authenticated caller.
type IssueImportSource struct {
	SetSHA256 string `json:"set_sha256"`
	Count     int    `json:"count"`
}

// IssueImportHeader declares the complete number of issue records in one file.
type IssueImportHeader struct {
	Count int `json:"count"`
}

// IssueImportRecord is the neutral, version-independent issue file record.
// Source identities remain uninterpreted text in the ordinary issue projection.
type IssueImportRecord struct {
	ID          int64           `json:"id"`
	Upstream    string          `json:"upstream"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       IssueState      `json:"state"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ClosedAt    *time.Time      `json:"closed_at"`
	Author      string          `json:"author"`
	Closer      string          `json:"closer"`
	History     []IssueHistory  `json:"history"`
	Kind        IssueKind       `json:"kind"`
	Repo        string          `json:"repo,omitempty"`
	Branch      string          `json:"branch,omitempty"`
	WorkStatus  IssueWorkStatus `json:"work_status"`
	Occurrences int64           `json:"occurrences"`
}

func (v IssueImportRecord) Issue(source IssueImportSource) Issue {
	return Issue{ID: v.ID, Revision: 1, Upstream: v.Upstream, Title: v.Title, Body: v.Body,
		State: v.State, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, ClosedAt: copyPointer(v.ClosedAt),
		Author: v.Author, Closer: v.Closer, SourceClosedAt: copyPointer(v.ClosedAt), History: slices.Clone(v.History), Imported: &source,
		Kind: v.Kind, Repo: v.Repo, Branch: v.Branch, WorkStatus: v.WorkStatus, Occurrences: v.Occurrences}
}

// Defaults supplies omitted display metadata; explicitly supplied timestamps survive.
func (v *IssueImportRecord) Defaults() {
	if v.Kind == "" {
		v.Kind = IssueManual
	}
	if v.WorkStatus == "" {
		v.WorkStatus = IssueTodo
	}
	if v.Occurrences == 0 {
		v.Occurrences = 1
	}
	if len(v.History) == 0 {
		v.History = nil
	}
	if !v.UpdatedAt.IsZero() {
		return
	}
	v.UpdatedAt = v.CreatedAt
	if v.ClosedAt != nil && v.ClosedAt.After(v.UpdatedAt) {
		v.UpdatedAt = *v.ClosedAt
	}
	for _, h := range v.History {
		if h.At.After(v.UpdatedAt) {
			v.UpdatedAt = h.At
		}
	}
}
