package model

import (
	"encoding/json"
	"time"
)

// RunTrigger is branch, promotion, schedule or tag.
type RunTrigger string

type AgentEvent struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	At          time.Time       `json:"at"`
	Upstream    string          `json:"upstream,omitempty"`
	IssueID     int64           `json:"issue_id,omitempty"`
	Repo        string          `json:"repo,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
	PromotionID string          `json:"promotion_id,omitempty"`
	Ref         string          `json:"ref,omitempty"`
	ObjectSHA   string          `json:"object_sha,omitempty"`
	CommitSHA   *string         `json:"commit_sha"`
	Summary     string          `json:"summary"`
	Data        json.RawMessage `json:"data,omitempty"`
}
