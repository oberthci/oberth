package model

import (
	"strings"
	"testing"
	"time"
)

func TestCIOutcomePreservesFailureOnPass(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		passed      bool
		previous    string
		wantBody    string
		occurrences int64
	}{
		{"failure replaces details", false, "earlier failure", "current failure", 4},
		{"pass retains details", true, "earlier failure", "earlier failure", 3},
		{"pass retains failure link", true, strings.Repeat("x", CITextBytes+1), "Read run_get failed-run for the complete failure details.", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := Issue{Body: test.previous, CIWorkID: "failed-run", Occurrences: 3}
			got := previous.WithCIOutcome(CIIssueOutcome{RunID: "current-run", Body: "current failure", Passed: test.passed, At: at})
			if got.Body != test.wantBody || got.Occurrences != test.occurrences {
				t.Fatalf("body/occurrences = %q/%d, want %q/%d", got.Body, got.Occurrences, test.wantBody, test.occurrences)
			}
			if test.passed {
				if got.State != IssueClosed || got.ClosedAt == nil || !got.ClosedAt.Equal(at) {
					t.Fatal("passing outcome did not close the issue at its recorded time", got)
				}
			} else if got.State != IssueOpen || got.ClosedAt != nil {
				t.Fatal("failing outcome did not leave the issue open", got)
			}
		})
	}
}

func TestCIOutcomeDefaultsOnlyInvalidWorkStatus(t *testing.T) {
	for _, test := range []struct{ input, want IssueWorkStatus }{
		{"", IssueTodo}, {"unknown", IssueTodo},
		{IssueTodo, IssueTodo}, {IssueActive, IssueActive},
		{IssueWaiting, IssueWaiting}, {IssueReview, IssueReview},
	} {
		t.Run(string(test.input), func(t *testing.T) {
			for _, passed := range []bool{false, true} {
				got := (Issue{WorkStatus: test.input}).WithCIOutcome(CIIssueOutcome{Passed: passed})
				if got.WorkStatus != test.want {
					t.Fatalf("work status for passed=%v: got %q, want %q", passed, got.WorkStatus, test.want)
				}
			}
		})
	}
}
