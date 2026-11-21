package model

import "slices"

const (
	MaxEventSummaryRunes = 512
	MaxEventDataBytes    = 8 << 10
	MaxEventPage         = 200
	MaxEventFilters      = 32
)

func (v AgentEvent) Clone() AgentEvent {
	v.CommitSHA = copyPointer(v.CommitSHA)
	v.Data = slices.Clone(v.Data)
	return v
}

// EventPage advances NextAfterID even when filtering produces an empty Events
// array. Gap is absent when no gap is known, true when continuity was lost.
type EventPage struct {
	Events      []AgentEvent `json:"events"`
	NextAfterID int64        `json:"next_after_id"`
	LatestID    int64        `json:"latest_id"`
	OldestID    int64        `json:"oldest_id"`
	HasMore     bool         `json:"has_more"`
	Gap         bool         `json:"gap,omitempty"`
}

func (v EventPage) Clone() EventPage {
	v.Events = slices.Clone(v.Events)
	for i := range v.Events {
		v.Events[i] = v.Events[i].Clone()
	}
	return v
}
