package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNativeCIIssueIsBoundedByConstruction(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	previous := Issue{ID: 42, Revision: 7, Kind: IssueCI, Title: strings.Repeat("<", 30000),
		Body: strings.Repeat("<", 30000), Lock: &IssueLock{Owner: "operator"},
		WorkStatus: IssueReview, CreatedAt: at.Add(-time.Hour), Occurrences: 3}
	for _, passed := range []bool{false, true} {
		got := previous.WithCIOutcome(CIIssueOutcome{Repo: "origin/team/repo", Ref: "refs/heads/main", RunID: "native-run", Body: strings.Repeat("<", 30000), Passed: passed, At: at})
		raw, err := json.Marshal(got)
		if err != nil || len(raw) > IssueBytes {
			t.Fatal("native projection can exceed its save budget", len(raw), err)
		}
		if got.ID != 42 || got.Revision != 8 || got.WorkStatus != IssueReview || got.CreatedAt != previous.CreatedAt || !strings.Contains(got.Body, "run_get") || got.Lock != previous.Lock || got.Title != "CI failed" {
			t.Fatal("native projection lost its bounded identity or run remedy")
		}
	}
}

func TestNativeCITitleBoundary(t *testing.T) {
	for _, size := range []int{CITextBytes, CITextBytes + 1} {
		title := strings.Repeat("x", size)
		got := (Issue{Title: title}).WithCIOutcome(CIIssueOutcome{})
		want := title
		if size > CITextBytes {
			want = "CI failed"
		}
		if got.Title != want {
			t.Fatal("wrong retained title at boundary", size)
		}
	}
}

func TestNativeCIBodyBoundary(t *testing.T) {
	for _, size := range []int{CITextBytes, CITextBytes + 1} {
		body := strings.Repeat("x", size)
		got := (Issue{}).WithCIOutcome(CIIssueOutcome{Body: body, RunID: "run-boundary"})
		want := body
		if size > CITextBytes {
			want = "Read run_get run-boundary for the complete failure details."
		}
		if got.Body != want {
			t.Fatal("wrong CI summary at boundary", size)
		}
	}
}
