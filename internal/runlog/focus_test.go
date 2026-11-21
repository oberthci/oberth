package runlog

import (
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestOmittedStepSelectsFailedThenRunningAndCanFollow(t *testing.T) {
	s := newStore(t, 4096)
	for _, step := range []string{"prepare", "failed", "running", "later"} {
		entry := native(step)
		entry.Step = step
		capture(t, s, entry, step+" output\n")
	}
	for _, tc := range []struct {
		name            string
		requested       Selector
		failed, running *string
		want            string
		follow          bool
	}{
		{"failed-first", Selector{}, new("failed"), new("running"), "failed output\n", false},
		{"running-fallback", Selector{}, nil, new("running"), "running output\n", true},
		{"failed-step-active-again", Selector{}, new("failed"), new("failed"), "failed output\n", true},
		{"explicit-step", Selector{Step: "prepare"}, new("failed"), new("running"), "prepare output\n", false},
		{"explicit-running", Selector{Step: "running"}, new("failed"), new("running"), "running output\n", true},
		{"explicit-attempt", Selector{NodeID: "later"}, new("failed"), new("running"), "later output\n", false},
		{"container-default", Selector{Container: "main"}, new("failed"), new("running"), "failed output\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			focus := SelectStep(tc.requested, tc.failed, tc.running)
			body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: focus.Selector, Filter: Filter{}})
			if err != nil || string(body) != tc.want || focus.Follow != tc.follow {
				t.Fatal(string(body), focus, err)
			}
			if focus.Selector.Container != tc.requested.Container {
				t.Fatal("container filter changed", focus)
			}
		})
	}
}

func TestSelectedRunningStepStaysTheTargetAsOutputArrives(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("one")
	entry.Step = "build"
	entry.State = Following
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	focus := SelectStep(Selector{}, nil, new("build"))
	_, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: focus.Selector, Filter: Filter{}})
	if !focus.Follow || publicerr.Classify(err).Reason() != "step is running" {
		t.Fatal(focus, err)
	}
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("arrived\n")), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: focus.Selector, Filter: Filter{}})
	if err != nil || string(body) != "arrived\n" {
		t.Fatal(string(body), err)
	}
}

func TestRootStepSelectionPreservesPresence(t *testing.T) {
	s := newStore(t, 1024)
	for _, step := range []string{"", "child"} {
		entry := native("node" + step)
		entry.Step = step
		capture(t, s, entry, "["+step+"]\n")
	}
	for _, requested := range []Selector{{}, {StepSet: true}} {
		focus := SelectStep(requested, new(""), new("child"))
		body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: focus.Selector, Filter: Filter{}})
		if err != nil || string(body) != "[]\n" || !focus.Selector.StepSet || focus.Follow {
			t.Fatal(string(body), focus, err)
		}
	}
	focus := SelectStep(Selector{}, nil, nil)
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: focus.Selector, Filter: Filter{}})
	if err != nil || string(body) != "[]\n[child]\n" || focus.Selector.StepSet || focus.Follow {
		t.Fatal(string(body), focus, err)
	}
}
