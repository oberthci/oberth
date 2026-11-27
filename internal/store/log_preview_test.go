package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogUsageProtectionPreviewIsReadOnly(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "preview")
	check := func(want string) {
		t.Helper()
		head := independentHead(t, s.readers)
		reason, err := s.LogProtection(t.Context(), r.ID)
		if err != nil || reason != want {
			t.Fatal(reason, err)
		}
		if independentHead(t, s.readers) != head {
			t.Fatal("preview wrote audit or purge intent")
		}
	}
	check("run is active")
	a := stateport.Action{Actor: "operator"}
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	check("archive eligibility unknown")
	requireOK(t, s.PreparePublication(t.Context(), a, publicationIntent(r)))
	check("publication protects logs")
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM purge`).Scan(&count))
	if count != 0 {
		t.Fatal("preview created purge intent")
	}
}
