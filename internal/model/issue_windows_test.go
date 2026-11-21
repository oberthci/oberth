package model

import (
	"testing"
	"time"
)

func TestIssueWindowsStaleAtExactWindowPerWorkStatus(t *testing.T) {
	updated := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	windows := IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}
	for _, test := range []struct {
		name    string
		state   IssueState
		status  IssueWorkStatus
		age     time.Duration
		windows IssueWindows
		stale   bool
	}{
		{"todo just before its window", IssueOpen, IssueTodo, 24*time.Hour - time.Nanosecond, windows, false},
		{"todo at its window", IssueOpen, IssueTodo, 24 * time.Hour, windows, true},
		{"review uses the general window", IssueOpen, IssueReview, 3 * time.Hour, windows, false},
		{"active just before its window", IssueOpen, IssueActive, 2*time.Hour - time.Nanosecond, windows, false},
		{"active at its window", IssueOpen, IssueActive, 2 * time.Hour, windows, true},
		{"closed is never stale", IssueClosed, IssueTodo, 100 * time.Hour, windows, false},
		{"zero windows mark nothing", IssueOpen, IssueActive, 100 * time.Hour, IssueWindows{}, false},
		{"zero active window spares active issues", IssueOpen, IssueActive, 100 * time.Hour, IssueWindows{General: time.Hour}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := Issue{State: test.state, WorkStatus: test.status, UpdatedAt: updated}
			if got := test.windows.Stale(v, updated.Add(test.age)); got != test.stale {
				t.Fatalf("stale = %v, want %v", got, test.stale)
			}
		})
	}
}
