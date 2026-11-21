package runlog

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestPurgeLeavesNoRecordAndReadUsesCallerFact(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("one"), "body\n")
	if _, err := s.Purge(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	assertNoPurgeEvidence(t, s)
	_, meta, err := s.Read(t.Context(), "run", ReadRequest{Purged: true})
	if publicerr.Classify(err).Reason() != "log was purged" || meta.Reason != "log was purged" {
		t.Fatal(meta, err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{})
	if err != nil || len(body) != 0 || meta.Reason != "run has produced no output yet" {
		t.Fatal(meta, err)
	}
	if err = s.Track(t.Context(), "run", []ContainerLog{native("next")}); err != nil {
		t.Fatal("storage repeats writer fence", err)
	}
	if err = s.Append(t.Context(), "run", []byte("new bytes")); err != nil {
		t.Fatal("storage repeats writer fence", err)
	}
}

func TestPlannedStepsWithoutPodsAreWaiting(t *testing.T) {
	s := newStore(t, 1024)
	for _, path := range []string{"build", ""} {
		request := ReadRequest{Selector: Selector{Step: path, StepSet: true}, PlannedSteps: []string{"build", ""}}
		_, meta, err := s.Read(t.Context(), "run", request)
		if publicerr.Classify(err).Code() != publicerr.InvalidTransition || meta.Reason != "step waiting to start" || meta.Follow {
			t.Fatal(path, meta, err)
		}
		if _, err = s.OpenStream(t.Context(), "run", request); publicerr.Classify(err).Reason() != "step waiting to start" {
			t.Fatal(err)
		}
	}
	_, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "outside"}, PlannedSteps: []string{"build", ""}})
	if publicerr.Classify(err).Code() != publicerr.NotFound || publicerr.Classify(err).Reason() != "step is unknown" {
		t.Fatal(err)
	}
}

func TestRunningReadOffersFollowWithoutPromisingStream(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("one")
	entry.State = Following
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	_, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}, PlannedSteps: []string{"build"}})
	if publicerr.Classify(err).Reason() != "step is running" || meta.Reason != "step is running" || !meta.Follow {
		t.Fatal(meta, err)
	}
}

func TestMixedCaptureReasonsCollapseWithoutLosingDetails(t *testing.T) {
	s := newStore(t, 1024)
	for _, tc := range []struct {
		node, body string
		gap        Gap
	}{
		{"missing", "", Gap{Reason: PodMissing, Detail: "Pod missing detail"}},
		{"partial", "kept\n", Gap{Detail: "Incomplete read detail"}},
	} {
		r, err := s.Reserve(t.Context(), "run", native(tc.node))
		if err != nil {
			t.Fatal(err)
		}
		var source io.ReadCloser
		if tc.body != "" {
			source = io.NopCloser(strings.NewReader(tc.body))
		}
		if err = r.Capture(t.Context(), CaptureRequest{Source: source, Gap: tc.gap}); err != nil {
			t.Fatal(err)
		}
	}
	for _, selector := range []Selector{{}, {Step: "build"}} {
		body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: selector})
		if err != nil || string(body) != "kept\n" || meta.Reason != "container output incomplete" || !meta.Truncated || meta.Detail != "Pod missing detail\nIncomplete read detail" {
			t.Fatal(string(body), meta, err)
		}
	}
}

func TestUnmatchedContainerDoesNotMakePlannedStepUnknown(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("one"), "body\n")
	_, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build", Container: "absent"}, PlannedSteps: []string{"build"}})
	if publicerr.Classify(err).Code() != publicerr.NotFound || meta.Reason != "no log matches this selector" {
		t.Fatal(meta, err)
	}
}

func assertNoPurgeEvidence(t *testing.T, s *Store) {
	t.Helper()
	for _, name := range []string{"run.log", "run.index.json"} {
		if _, err := os.Stat(filepath.Join(s.directory, name)); !os.IsNotExist(err) {
			t.Fatal("purge left evidence", name, err)
		}
	}
	usage, err := s.UsageByRun(t.Context())
	if err != nil || len(usage) != 0 {
		t.Fatal(usage, err)
	}
}
func TestPurgeWithoutStoredOutputCreatesNothing(t *testing.T) {
	s := newStore(t, 1)
	if removed, err := s.Purge(t.Context(), "run"); err != nil || removed != 0 {
		t.Fatal(removed, err)
	}
	assertNoPurgeEvidence(t, s)
}
func TestSuppliedPlanOwnsWhetherStepIsKnown(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("one"), "body\n")
	_, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}, PlannedSteps: []string{"other"}})
	if publicerr.Classify(err).Code() != publicerr.NotFound || meta.Reason != "step is unknown" {
		t.Fatal(meta, err)
	}
}
