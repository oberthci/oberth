package model

import (
	"testing"
	"time"
)

func TestIssueImportDefaultsUsesLatestSourceTime(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	closed, latest := created.Add(time.Hour), created.Add(3*time.Hour)
	for _, test := range []struct {
		name   string
		record IssueImportRecord
		want   time.Time
	}{
		{"creation only", IssueImportRecord{CreatedAt: created}, created},
		{"closure latest", IssueImportRecord{CreatedAt: created, ClosedAt: &closed, History: []IssueHistory{{At: created}}}, closed},
		{"history unordered", IssueImportRecord{CreatedAt: created, ClosedAt: &closed, History: []IssueHistory{{At: latest}, {At: closed}}}, latest},
		{"creation latest", IssueImportRecord{CreatedAt: latest, ClosedAt: &closed, History: []IssueHistory{{At: created}}}, latest},
		{"explicit update", IssueImportRecord{CreatedAt: created, UpdatedAt: closed, History: []IssueHistory{{At: latest}}}, closed},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.record.Defaults()
			if !test.record.UpdatedAt.Equal(test.want) {
				t.Fatalf("updated at = %s, want %s", test.record.UpdatedAt, test.want)
			}
		})
	}
}
