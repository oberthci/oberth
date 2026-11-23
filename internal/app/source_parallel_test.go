package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Every decision must enter before any can finish. The deadline only stops a
// broken implementation from hanging; success makes no wall-clock assertion.
func TestAdmissionStartsDistinctPinsTogether(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var pins []sourcecontract.Descriptor
	var repositories []string
	for i := range 3 {
		repo := fmt.Sprintf("origin/team/producer%d", i)
		pin := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: repo, Commit: strings.Repeat("e", 40)}
		pins = append(pins, pin, pin)
		repositories = append(repositories, repo)
	}
	f.d.SourcePolicy, _ = json.Marshal(map[string][]string{request().Repository: repositories})
	f.setDocument(pinnedWorkflow(pins...))
	var calls atomic.Int32
	entered := make(chan struct{})
	f.source.Reachable = func(ctx context.Context, _ sourcecontract.Revision) (int64, bool, error) {
		if calls.Add(1) == 3 {
			close(entered)
		}
		select {
		case <-entered:
			return 2, true, nil
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prepared, err := f.new(t).Admit(ctx, request(), f.source)
	if err != nil || calls.Load() != 3 || len(prepared.View().SourceBindings) != len(pins) {
		t.Fatal("distinct decisions did not overlap exactly once", calls.Load(), err)
	}
}

func TestFiveAllowedPinsAreRefusedBeforeAnyDecision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(`{"origin/team/widget":["origin/team/source"]}`)
	var pins []sourcecontract.Descriptor
	for i := range 5 {
		pins = append(pins, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: fmt.Sprintf("%040x", i+1)})
	}
	f.setDocument(pinnedWorkflow(pins...))
	var calls atomic.Int32
	f.source.Reachable = func(context.Context, sourcecontract.Revision) (int64, bool, error) {
		calls.Add(1)
		return 2, true, nil
	}
	prepared, err := f.new(t).Admit(t.Context(), request(), f.source)
	if prepared != nil || err == nil || !strings.Contains(err.Error(), "more than four source pins are declared") || calls.Load() != 0 {
		t.Fatal("pinned-destination limit did not stop fan-out", calls.Load(), err)
	}
}

func TestDecidedPinRefusalSurvivesPreparationCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(`{"origin/team/widget":["origin/team/source"]}`)
	pin := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: strings.Repeat("e", 40)}
	slow := pin
	slow.Commit = strings.Repeat("f", 40)
	f.setDocument(pinnedWorkflow(pin, slow))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.source.Reachable = func(ctx context.Context, at sourcecontract.Revision) (int64, bool, error) {
		if at.Commit == pin.Commit {
			cancel()
			return 2, false, nil
		}
		<-ctx.Done()
		return 0, false, ctx.Err()
	}
	prepared, err := f.new(t).Admit(ctx, request(), f.source)
	expectRefusal(t, err, "reachable")
	if prepared != nil {
		t.Fatal("cancelled admission was sealed")
	}
}
