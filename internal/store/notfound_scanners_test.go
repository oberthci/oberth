package store

import (
	"database/sql"
	"errors"
	"testing"
)

type missingReviewRow struct{}

func (missingReviewRow) Scan(...any) error { return sql.ErrNoRows }
func TestRowScannersTranslateNotFound(t *testing.T) {
	tests := map[string]func() error{
		"run": func() error { _, e := scanRun(missingReviewRow{}); return e }, "step": func() error { _, e := scanStepResult(missingReviewRow{}); return e }, "publication": func() error { _, e := scanPublication(missingReviewRow{}); return e }, "promotion": func() error { _, e := scanPromotion(missingReviewRow{}); return e }, "issue": func() error { _, e := scanIssue(missingReviewRow{}); return e }, "receive": func() error { _, e := scanReceiveEvent(missingReviewRow{}); return e },
	}
	for name, scan := range tests {
		t.Run(name, func(t *testing.T) {
			if err := scan(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scanner leaks driver sentinel: %v", err)
			}
		})
	}
}
