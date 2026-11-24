package runlog

import (
	"io"
	"strings"
	"testing"
)

func TestFollowingAvailabilityNeedsCurrentObservation(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("prior"), "kept\n")
	entry := native("live")
	entry.State = Following
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	assertFollowingRead(t, s, true)
	s.KeepFollowing("run", nil)
	assertFollowingRead(t, s, false)
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	assertFollowingRead(t, s, true)
	reopened, err := Open(t.Context(), s.directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertFollowingRead(t, reopened, false)
}

func assertFollowingRead(t *testing.T, s *Store, following bool) {
	t.Helper()
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}})
	if err != nil || string(body) != "kept\n" || meta.Follow != following || meta.Copying {
		t.Fatal("orphaned source is still followed or retained history changed", string(body), meta, err)
	}
}

func TestRetiringFollowDoesNotRevokeAnActiveCopy(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("live")
	entry.State = Following
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	s.KeepFollowing("run", nil)
	_, meta, _ := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}})
	if !meta.Copying || meta.Follow {
		t.Fatal("retiring follow revoked a reserved copy", meta)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("retained\n"))}); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}})
	if err != nil || string(body) != "retained\n" || meta.Follow || meta.Copying {
		t.Fatal("late capture changed after retirement", string(body), meta, err)
	}
}

func TestReleaseDoesNotReviveARetiredFollow(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("live")
	entry.State = Following
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	s.KeepFollowing("run", nil)
	if err = r.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, meta, _ := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}})
	if meta.Follow || meta.Copying || meta.Reason != pendingReason {
		t.Fatal("release revived an absent live source", meta)
	}
}

func TestFollowRetirementIsScopedToMissingPhysicalSources(t *testing.T) {
	s := newStore(t, 1024)
	first, second := native("one"), native("two")
	first.State, second.State = Following, Following
	for _, id := range []string{"run", "other"} {
		if err := s.Track(t.Context(), id, []ContainerLog{first, second}); err != nil {
			t.Fatal(err)
		}
	}
	s.KeepFollowing("run", []ContainerLog{second})
	for _, tc := range []struct {
		run, node string
		follow    bool
	}{{"run", "one", false}, {"run", "two", true}, {"other", "one", true}} {
		_, meta, _ := s.Read(t.Context(), tc.run, ReadRequest{Selector: Selector{NodeID: tc.node, Container: "main"}})
		if meta.Follow != tc.follow {
			t.Fatal("follow retirement crossed a physical binding", tc, meta)
		}
	}
}
