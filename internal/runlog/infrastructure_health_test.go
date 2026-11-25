package runlog

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestFailedInfrastructureWriteInvalidatesReadableOldEvidence(t *testing.T) {
	s := newStore(t, 4096)
	r := InfrastructureReceipt{RunID: "run", WorkflowUID: "workflow", NodeID: "node", PodUID: "pod", Kind: "secret", StartedAt: time.Unix(100, 0), FinishedAt: time.Unix(101, 0)}
	if err := s.RecordInfrastructure(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.directory, 0700) })
	success := r
	success.Delivered, success.FinishedAt = true, time.Unix(102, 0)
	if err := s.RecordInfrastructure(t.Context(), success); err == nil {
		t.Fatal("write failure was not injected")
	}
	if err := os.Chmod(s.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if saved, err := s.load(r.RunID); err != nil || len(saved.Infrastructure) != 1 || saved.Infrastructure[0].Delivered {
		t.Fatal("test did not leave readable stale evidence", saved.Infrastructure, err)
	}
	// Even a later successful older writer cannot recover a missed tombstone.
	if err := s.RecordInfrastructure(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	assertLostDeliveryCertainty(t, s, r.RunID)
}

func assertLostDeliveryCertainty(t *testing.T, s *Store, id string) {
	t.Helper()
	if got, err := s.Infrastructure(t.Context(), id); err != nil || len(got) != 0 {
		t.Fatal("failed tombstone exposed stale infrastructure", got, err)
	}
	if got, err := s.Infrastructure(t.Context(), "healthy-run"); err != nil || len(got) != 0 {
		t.Fatal("unrelated run lost certainty", got, err)
	}
	if _, err := s.Purge(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Infrastructure(t.Context(), id); err != nil || len(got) != 0 {
		t.Fatal("purge retained uncertainty", got, err)
	}
}

func TestReceiptWriteFailuresStayInTheirRun(t *testing.T) {
	s := newStore(t, 4096)
	quiet := InfrastructureReceipt{RunID: "quiet", WorkflowUID: "workflow", NodeID: "node", PodUID: "pod", Kind: "secret", StartedAt: time.Unix(100, 0), FinishedAt: time.Unix(101, 0)}
	if err := s.RecordInfrastructure(t.Context(), quiet); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.directory, 0700) })
	noisy := quiet
	noisy.RunID = "noisy"
	loseReceipts(t, s, noisy, maxVolatileRunReceipts+1)
	if _, err := s.Infrastructure(t.Context(), "noisy"); err == nil {
		t.Fatal("run beyond its bound claimed certain evidence")
	}
	for id, want := range map[string]int{"quiet": 1, "author-run": 0} {
		if got, err := s.Infrastructure(t.Context(), id); err != nil || len(got) != want {
			t.Fatal("another run's receipt failures changed this run's evidence", id, got, err)
		}
	}
	if err := os.Chmod(s.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge(t.Context(), "noisy"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Infrastructure(t.Context(), "noisy"); err != nil || len(got) != 0 {
		t.Fatal("purge kept the run uncertain", got, err)
	}
}

func loseReceipts(t *testing.T, s *Store, base InfrastructureReceipt, n int) {
	t.Helper()
	for i := range n {
		lost := base
		lost.NodeID = fmt.Sprint("node-", i)
		if err := s.RecordInfrastructure(t.Context(), lost); err == nil {
			t.Fatal("write failure was not injected")
		}
	}
}

func TestDeliveryEvidenceHealthHasBoundedMemory(t *testing.T) {
	h := deliveryEvidenceHealth{}
	lost := errors.New("write failed")
	for i := range maxVolatileDeliveryReceipts {
		h.remember(InfrastructureReceipt{RunID: fmt.Sprintf("run-%d", i/32), NodeID: fmt.Sprint(i)}, lost, true)
	}
	h.remember(InfrastructureReceipt{RunID: "late"}, lost, true)
	if _, err := h.merge("late", nil); h.count != maxVolatileDeliveryReceipts || err == nil {
		t.Fatal("uncertainty either grew unbounded or falsely claimed certainty")
	}
	if _, err := h.merge("untracked", nil); err != nil {
		t.Fatal("a full fallback made an unrelated run uncertain", err)
	}
	h.purge("run-0")
	// A lost success with no failure to mask is not retained.
	h.remember(InfrastructureReceipt{RunID: "success", Delivered: true}, lost, false)
	h.remember(InfrastructureReceipt{RunID: "fresh"}, lost, true)
	if got, err := h.merge("fresh", nil); err != nil || len(got) != 1 || !got[0].Uncertain {
		t.Fatal("fallback stayed latched after its count dropped", got, err)
	}
	if h.count != maxVolatileDeliveryReceipts-31 {
		t.Fatal("a lost success with nothing to mask was retained", h.count)
	}
	for i := range maxUncertainDeliveryRuns + 1 {
		h.markUncertain(fmt.Sprint("uncertain-", i))
	}
	if len(h.uncertain) != maxUncertainDeliveryRuns {
		t.Fatal("uncertain runs grew unbounded", len(h.uncertain))
	}
}
