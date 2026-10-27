package model

import "time"

// SchedulePause records who paused a schedule entry, when and why. It is
// derived from the audit chain: the latest schedule.pause or schedule.resume
// action for the entry decides, so a pause needs no schema of its own.
type SchedulePause struct {
	By     string
	At     time.Time
	Reason string
}

// ScheduleFireSpec is the cursor snapshot used to decide a cron entry is due.
// HasPrevious distinguishes an absent cursor from a known-due zero cursor.
// Repo is the canonical upstream/org/repo key, checked against the run's repo.
type ScheduleFireSpec struct {
	Repo        string
	At          time.Time
	Previous    time.Time
	HasPrevious bool
}

// ScheduleEnqueueResult reports an atomic cron decision. Outcome is fired,
// coalesced (an active run already covers this entry/branch), or stale (another
// transaction consumed the cursor snapshot; no state was changed).
type ScheduleEnqueueResult struct {
	Enqueued EnqueueRunResult
	Outcome  string
}
