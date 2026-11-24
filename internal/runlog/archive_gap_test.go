package runlog

import (
	"io"
	"testing"
)

func TestCapturedPartialReceiptsPermitCollectionWithoutRewritingBytes(t *testing.T) {
	s := newStore(t, 4096)
	entry := native("one")
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(&brokenSource{})}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Containers(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.Archived(t.Context(), "run", []ContainerLog{entry})
	if err != nil || !ready || before[0].Reason == "" {
		t.Fatal("durable partial receipt blocks collection", ready, before, err)
	}
	if err = s.RecordGap(t.Context(), "run", entry, Gap{Reason: PodMissing}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Containers(t.Context(), "run")
	if err != nil || after[0] != before[0] {
		t.Fatal("collection overwrote retained partial evidence", after, before, err)
	}
}
