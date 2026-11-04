package model

// QueueCapStatus describes the optional branch-push admission cap. Deferred
// receives have no run yet and are counted separately from not-started runs.
type QueueCapStatus struct {
	MaxNotStarted int    `json:"maxNotStarted"`
	NotStarted    int    `json:"notStarted"`
	Deferred      int    `json:"deferred"`
	Reason        string `json:"reason,omitempty"`
}
