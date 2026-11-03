package store

import (
	"strings"
	"testing"
	"time"
)

func TestLegacyRetirementNamesLiveRunsAndPreservesJournal(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.LegacyRetirementBlockers(t.Context())
	if err != nil || len(rows) == 0 || !strings.Contains(strings.Join(rows, ","), run.ID) {
		t.Fatalf("queued run omitted: %v %v", rows, err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET status='interrupted',finished_at=updated_at WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO vm_capacity_slots(slot,run_id) VALUES(1,?)`, run.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.LegacyRetirementBlockers(t.Context())
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0], "capacity") {
		t.Fatalf("slot omitted: %v %v", rows, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM vm_capacity_slots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("read changed ownership: %d %v", count, err)
	}
}
