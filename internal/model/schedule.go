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
