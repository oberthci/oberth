package argojob

import (
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
	"time"
)

func TestObservationAndLogWorkerBounds(t *testing.T) {
	t.Parallel()
	if observationAttempts != 5 || captureBudget != 30*time.Second || captureWorkers != 2 {
		t.Fatal(observationAttempts, captureBudget, captureWorkers)
	}
}

func TestTerminalEvidenceWindowBoundary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	e := New(Config{Now: func() time.Time { return now }})
	defer e.queue.ShutDown()
	tracked := &tracking{completed: true, evidenceUntil: start.Add(terminalEvidenceGrace)}
	o := Observation{Nodes: []stateport.Node{{NodeID: "native"}}}
	if terminalEvidenceGrace != 60*time.Second || e.evidenceSettled(tracked, o, nil) {
		t.Fatal("incomplete evidence settled immediately")
	}
	for _, tc := range []struct {
		elapsed time.Duration
		settled bool
	}{{60*time.Second - 1, false}, {60 * time.Second, true}, {60*time.Second + 1, true}} {
		now = start.Add(tc.elapsed)
		if e.evidenceSettled(tracked, o, nil) != tc.settled {
			t.Fatal("late evidence deadline", tc)
		}
	}
}
func TestPublicRefusalCatalogue(t *testing.T) {
	t.Parallel()
	catalogue := []error{absent(nil), ownership(nil), unavailable(nil), accountMissing(false, nil), accountMissing(true, nil), notHeld(nil), limited()}
	for _, err := range catalogue {
		var public *publicerr.Error
		if !errors.As(err, &public) {
			t.Fatal(err)
		}
		if publicerr.CheckMessage(public.Reason(), 80) != nil || publicerr.CheckMessage(public.Remedy(), 120) != nil {
			t.Fatal(public)
		}
	}
}
func TestObservationBackoffGivesTransientRefusalsTime(t *testing.T) {
	t.Parallel()
	e := New(Config{})
	defer e.queue.ShutDown()
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := e.config.RateLimiter.When("run"); got != want {
			t.Fatal(got, want)
		}
	}
}
func TestPublishedExecLimits(t *testing.T) {
	t.Parallel()
	if MaxExecOutput != 4<<20 {
		t.Fatal(MaxExecOutput)
	}
	if DefaultExecTimeout != time.Minute || DefaultExecOutput != 64<<10 {
		t.Fatal("published defaults changed")
	}
}
