package runlog

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestPublicReadExplainsPendingAndPurgedStates(t *testing.T) {
	s := newStore(t, 1024)
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || len(body) != 0 || !meta.ScanComplete || meta.Reason != "run has produced no output yet" {
		t.Fatal(meta, err)
	}
	entry := native("one")
	if err = s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	assertReadRefusal(t, s, "step waiting to start")
	entry.State = Following
	if err = s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	assertReadRefusal(t, s, "step is running")
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	assertReadRefusal(t, s, "step output is being copied")
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("body\n")), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Purge(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	assertReadRefusal(t, s, "log was purged", ReadRequest{Selector: Selector{Step: "build"}, Purged: true})
	reopened, err := Open(t.Context(), s.directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertReadRefusal(t, reopened, "log was purged", ReadRequest{Selector: Selector{Step: "build"}, Purged: true})
}
func assertReadRefusal(t *testing.T, s *Store, reason string, facts ...ReadRequest) {
	t.Helper()
	request := ReadRequest{Selector: Selector{Step: "build"}}
	if len(facts) > 0 {
		request = facts[0]
	}
	_, _, err := s.Read(t.Context(), "run", request)
	e := publicerr.Classify(err)
	if err == nil || e.Code() != publicerr.InvalidTransition || e.Reason() != reason {
		t.Fatal(reason, err)
	}
}
func TestOnlyUnknownStepIsNotFound(t *testing.T) {
	s := newStore(t, 1024)
	_, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "missing"}, Filter: Filter{}})
	e := publicerr.Classify(err)
	if e.Code() != publicerr.NotFound || e.Reason() != "step is unknown" {
		t.Fatal(err)
	}
}
func TestPublicReadExplainsContainerCuts(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		gap                Gap
		limit              int64
	}{
		{"cap", "long output", "run log byte limit reached", Gap{}, 3},
		{"pod", "", "pod missing", Gap{Reason: PodMissing, Detail: "native Pod disappeared"}, 1024},
		{"gap", "body", "container output incomplete", Gap{Detail: "capture gap supplied by caller"}, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, tc.limit)
			r, err := s.Reserve(t.Context(), "run", native("one"))
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
			for _, selector := range []Selector{{}, {Step: "build"}, {NodeID: "one", Container: "main"}} {
				_, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: selector, Filter: Filter{}})
				if err != nil || !meta.Truncated || meta.Reason != tc.reason || meta.Detail == "" {
					t.Fatal(meta, err)
				}
			}
		})
	}
}

func TestStepReadSelectsMainContainers(t *testing.T) {
	s := newStore(t, 1024)
	for _, name := range []string{"init", "main", "sidecar", "user"} {
		entry := native("one")
		entry.Container = name
		entry.Main = name == "main" || name == "user"
		capture(t, s, entry, name+"\n")
	}
	for _, tc := range []struct {
		selector Selector
		want     string
	}{
		{Selector{Step: "build"}, "main\nuser\n"},
		{Selector{NodeID: "one"}, "main\nuser\n"},
		{Selector{Step: "build", Container: "init"}, "init\n"},
		{Selector{}, "init\nmain\nsidecar\nuser\n"},
	} {
		body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: tc.selector, Filter: Filter{}})
		if err != nil || string(body) != tc.want {
			t.Fatal(string(body), err)
		}
	}
}
func TestOpenedRootSurvivesRename(t *testing.T) {
	s := newStore(t, 1024)
	moved := s.directory + "-moved"
	if err := os.Rename(s.directory, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, s.directory); err != nil {
			t.Error(err)
		}
	})
	if err := s.Append(t.Context(), "run", []byte("body\n")); err != nil {
		t.Fatal(err)
	}
	capture(t, s, native("one"), "source\n")
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != "body\nsource\n" {
		t.Fatal(string(body), err)
	}
}
func TestReadRejectsSymlinkWithinLogRoot(t *testing.T) {
	s := newStore(t, 1024)
	if err := s.Append(t.Context(), "real", []byte("body\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.log", filepath.Join(s.directory, "alias.log")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(t.Context(), "alias", ReadRequest{Selector: Selector{}, Filter: Filter{}}); err == nil {
		t.Fatal("symlink body followed")
	}
}

func TestStepReadKeepsCapturedAttemptWhileNextWaits(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("earlier"), "past output\n")
	if err := s.Track(t.Context(), "run", []ContainerLog{native("next")}); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}, Filter: Filter{}})
	if err != nil || string(body) != "past output\n" {
		t.Fatal(string(body), err)
	}
}
func TestEmptyReceiptAtSharedOffsetDoesNotOverlap(t *testing.T) {
	s := newStore(t, 1024)
	first, err := s.Reserve(t.Context(), "run", native("first"))
	if err != nil {
		t.Fatal(err)
	}
	capture(t, s, native("empty"), "")
	if err = first.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("body\n")), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "build"}, Filter: Filter{}})
	if err != nil || string(body) != "body\n" {
		t.Fatal(string(body), err)
	}
}

func TestRestartedCopyIsIncompleteRatherThanStillCopying(t *testing.T) {
	s := newStore(t, 1024)
	if _, err := s.Reserve(t.Context(), "run", native("one")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), s.directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertReadRefusal(t, reopened, "container capture incomplete")
}
func TestReleaseRestoresFollowingAvailability(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("one")
	entry.State = Following
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertReadRefusal(t, s, "step is running")
}

type measuredSource struct {
	source io.Reader
	reads  int
}

func (s *measuredSource) Read(p []byte) (int, error) { s.reads++; return s.source.Read(p) }
func (*measuredSource) Close() error                 { return nil }
func TestRepeatedCaptureDoesNotReadAnotherSource(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("first")), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	source := &measuredSource{source: strings.NewReader("second")}
	err = r.Capture(t.Context(), CaptureRequest{Source: source, Gap: Gap{}})
	if err == nil || source.reads != 0 || publicerr.Classify(err).Code() != publicerr.InvalidTransition {
		t.Fatal(source.reads, err)
	}
}

func TestReserveRefinesAttemptWithoutReopening(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("parent")
	if _, err := s.Reserve(t.Context(), "run", entry); err != nil {
		t.Fatal(err)
	}
	entry.NodeID, entry.ParentNodeID, entry.Step = "child", "parent", "build/child"
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil || r != nil {
		t.Fatal("refinement reopened source", err)
	}
	entries, err := s.Containers(t.Context(), "run")
	if err != nil || entries[0].NodeID != "child" || entries[0].Step != "build/child" || entries[0].State != Copying {
		t.Fatal(entries, err)
	}
}
