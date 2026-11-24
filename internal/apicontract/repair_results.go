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
// Stale lists ended runs the workflow controller still counts as holders;
// Remedy and Command then say how an administrator releases them.
type LockState struct {
	Lock    string       `json:"lock"`
	Holders []LockHolder `json:"holders"`
	Held    int          `json:"held"`
	Limit   *int         `json:"limit"`
	Waiters []string     `json:"waiters"`
	Stale   []string     `json:"stale,omitempty"`
	Remedy  string       `json:"remedy,omitempty"`
	Command string       `json:"command,omitempty"`
}
type SyncStatus struct {
	Locks []LockState `json:"locks"`
}

// LockHolder counts permits, including separate live steps of the same run.
type LockHolder struct {
	RunID   string `json:"run_id"`
	Permits int    `json:"permits"`
	Steps   string `json:"steps"`
}
