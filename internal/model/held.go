package model

import "time"

// HeldNotice describes one observed hold entry. It carries no outcome or live countdown.
type HeldNotice struct {
	RunID      string    `json:"run_id"`
	Step       string    `json:"step"`
	NodeID     string    `json:"node_id"`
	PodUID     string    `json:"pod_uid"`
	HeldSince  time.Time `json:"held_since,omitzero"`
	TTLSeconds int64     `json:"ttl_seconds,omitempty"`
}

// LiveHold is a status reply from the helper; availability is queried for each call.
type LiveHold struct {
	HeldNotice
	PodName             string `json:"pod_name"`
	RemainingMS         int64  `json:"remaining_ms,omitempty"`
	AvailabilityUnknown bool   `json:"availability_unknown,omitempty"`
}
