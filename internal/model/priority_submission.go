package model

import "time"

// PriorityRequest is server-side submission metadata. UnscopedAdmin is set
// only from an authenticated API actor; transports resolve their exact key in
// the enqueue transaction when it is nil.
type PriorityRequest struct {
	Priority             RunPriority `json:"priority,omitempty"`
	PriorityReason       string      `json:"priority_reason,omitempty"`
	RequesterFingerprint string      `json:"-"`
	UnscopedAdmin        *bool       `json:"-"`
	Supersede            bool        `json:"-"`
}

type PriorityGrant struct {
	Identity  string      `json:"identity"`
	MaxClass  RunPriority `json:"max_class"`
	Expires   time.Time   `json:"expires"`
	GrantedBy string      `json:"granted_by"`
}
