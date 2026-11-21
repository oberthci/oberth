package apicontract

type OutboxRecord struct {
	ID            string `json:"id"`
	Repo          string `json:"repo"`
	State         string `json:"state"`
	DisplayReason string `json:"display_reason"`
	Detail        string `json:"detail,omitempty"`
}
type OutboxResult struct {
	Records []OutboxRecord `json:"records"`
}

// LockState contains only current authorized native holders and waiters.
type LockState struct {
	Lock    string   `json:"lock"`
	Holders []string `json:"holders"`
	Waiters []string `json:"waiters"`
}
type SyncStatus struct {
	Locks []LockState `json:"locks"`
}
