package runlog

import (
	"io"
	"strings"
	"testing"
)

func TestFailedAttemptLogSpanBeforeFilterAndStream(t *testing.T) {
	t.Parallel()
	s := newStore(t, 4096)
	marker := "oberth-hold: rerunning on ffffffff"
	capture(t, s, native("held"), "P0 first\nP0 failed\n"+marker+"\nF passed\n")
	q := ReadRequest{Selector: Selector{NodeID: "held", Container: "main"}, BeforeMarker: marker, Filter: Filter{Tail: true, Limit: 1}}
	body, meta, err := s.Read(t.Context(), "run", q)
	if err != nil || string(body) != "P0 failed\n" || meta.Follow {
		t.Fatal(string(body), meta, err)
	}
	q.Filter = Filter{Pattern: "F passed"}
	body, _, err = s.Read(t.Context(), "run", q)
	if err != nil || len(body) != 0 {
		t.Fatal("F output escaped prefix", string(body), err)
	}
	stream, err := s.OpenStream(t.Context(), "run", q)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	body, err = io.ReadAll(stream)
	if err != nil || string(body) != "P0 first\nP0 failed\n" {
		t.Fatal("stream escaped prefix", string(body), err)
	}
	q.BeforeMarker = "missing marker"
	body, meta, err = s.Read(t.Context(), "run", q)
	if err != nil || len(body) != 0 || !strings.Contains(meta.Reason, "pending") {
		t.Fatal("missing boundary exposed F", string(body), meta, err)
	}
	checkAfterMarker(t, s, marker)
}

func checkAfterMarker(t *testing.T, s *Store, marker string) {
	t.Helper()
	q := ReadRequest{Selector: Selector{NodeID: "held", Container: "main"}, AfterMarker: marker}
	body, _, err := s.Read(t.Context(), "run", q)
	if err != nil || string(body) != "F passed\n" {
		t.Fatal("successor span includes carried output", string(body), err)
	}
}

func TestFailedLogSpanKeepsPartialLineAndExactEmptyAttempt(t *testing.T) {
	t.Parallel()
	marker := "oberth-hold: rerunning on ffffffff as next"
	for _, failed := range []string{"partial P0", ""} {
		s := newStore(t, 4096)
		capture(t, s, native("held"), failed+marker+"\nF output\n")
		body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{NodeID: "held", Container: "main"}, BeforeMarker: marker})
		if err != nil || string(body) != failed || meta.Reason == "failed-attempt log pending" {
			t.Fatal("failed log boundary lost bytes", failed, string(body), meta, err)
		}
	}
}
