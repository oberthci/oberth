package model

import "time"

// EvidencePurge is a durable audit projection; run and step rows are retained.
type EvidencePurge struct {
	ID     string    `json:"purge_id"`
	RunID  string    `json:"run_id"`
	What   string    `json:"what"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"purged_at"`
	Reason string    `json:"reason"`
	Bytes  int64     `json:"bytes"`
	State  string    `json:"state"`
	Error  string    `json:"error,omitempty"`
}
