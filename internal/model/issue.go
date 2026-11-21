package model

import (
	"slices"
	"time"
)

// IssueKind distinguishes authored issues from CI observations.
type IssueKind string

const (
	IssueManual IssueKind = "manual"
	IssueCI     IssueKind = "ci"
)

func (v IssueKind) Valid() bool { return v == IssueManual || v == IssueCI }

// IssueState is the durable open/closed state, separate from work status.
type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

func (v IssueState) Valid() bool { return v == IssueOpen || v == IssueClosed }

type IssueWorkStatus string

const (
	IssueTodo    IssueWorkStatus = "todo"
	IssueActive  IssueWorkStatus = "active"
	IssueWaiting IssueWorkStatus = "waiting"
	IssueReview  IssueWorkStatus = "review"
)

func (v IssueWorkStatus) Valid() bool {
	return v == IssueTodo || v == IssueActive || v == IssueWaiting || v == IssueReview
}

// Issue is the public issue record. Stale and Lock are read observations;
// observing them never changes revision, lock expiry or timestamps.
type Issue struct {
	// Author is the native creator or immutable source attribution on imported issues.
	// Closer and SourceClosedAt are immutable source attribution.
	SourceClosedAt *time.Time         `json:"source_closed_at,omitempty"`
	Imported       *IssueImportSource `json:"imported,omitempty"`
	Author         string             `json:"author,omitempty"`
	Closer         string             `json:"closer,omitempty"`
	History        []IssueHistory     `json:"history,omitempty"`
	ID             int64              `json:"id"`
	Revision       int64              `json:"revision"`
	Kind           IssueKind          `json:"kind"`
	Title          string             `json:"title"`
	Body           string             `json:"body"`
	State          IssueState         `json:"state"`
	WorkStatus     IssueWorkStatus    `json:"work_status"`
	Upstream       string             `json:"upstream"`
	Repo           string             `json:"repo"`
	Branch         string             `json:"branch"`
	Occurrences    int64              `json:"occurrences"`
	CIOrigin       string             `json:"ci_origin"`
	CIWorkID       string             `json:"ci_work_id"`
	Stale          bool               `json:"stale"`
	Lock           *IssueLock         `json:"lock"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	ClosedAt       *time.Time         `json:"closed_at"`
}

func (v Issue) Clone() Issue {
	v.Imported = copyPointer(v.Imported)
	v.SourceClosedAt = copyPointer(v.SourceClosedAt)
	v.History = slices.Clone(v.History)
	v.Lock = copyPointer(v.Lock)
	v.ClosedAt = copyPointer(v.ClosedAt)
	return v
}

// IssueLock is an explicitly acquired coordination lock.
type IssueLock struct {
	Owner      string    `json:"owner"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}
