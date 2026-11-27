package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func newStore(t *testing.T, limit int64) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir(), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func native(node string) ContainerLog {
	return ContainerLog{NodeID: node, Step: "build", PodUID: "pod-" + node, PodName: "name-" + node, Namespace: "runs", Container: "main", Main: true}
}
func capture(t *testing.T, s *Store, entry ContainerLog, body string) {
	t.Helper()
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil || r == nil {
		t.Fatal("reserve", r, err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader(body)), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
}
func TestCaptureOnceEmptySpansAndRestart(t *testing.T) {
	s := newStore(t, 4096)
	capture(t, s, native("one"), "first\n")
	capture(t, s, native("two"), "")
	entries, err := s.Containers(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Start != 6 || entries[1].End != 6 || entries[1].State != Captured {
		t.Fatal(entries)
	}
	reopened, err := Open(t.Context(), s.directory, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	r, err := reopened.Reserve(t.Context(), "run", native("one"))
	if err != nil || r != nil {
		t.Fatal("copy reopened", err)
	}
	body, _, err := reopened.Read(t.Context(), "run", ReadRequest{Selector: Selector{NodeID: "one"}, Filter: Filter{}})
	if err != nil || string(body) != "first\n" {
		t.Fatal(string(body), err)
	}
}
func TestReservationsSurviveCrashPoints(t *testing.T) {
	for _, stage := range []string{"reserved", "spooled", "appended", "published"} {
		t.Run(stage, func(t *testing.T) {
			s := newStore(t, 4096)
			entry := native("one")
			r, err := s.Reserve(t.Context(), "run", entry)
			if err != nil {
				t.Fatal(err)
			}
			simulateCrashPoint(t, s, r, stage)
			reopened, err := Open(t.Context(), s.directory, 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			retry, err := reopened.Reserve(t.Context(), "run", entry)
			if err != nil || retry != nil {
				t.Fatal("crash allowed another source open", err)
			}
		})
	}
}
func simulateCrashPoint(t *testing.T, s *Store, r *Reservation, stage string) {
	t.Helper()
	switch stage {
	case "spooled":
		file, err := os.CreateTemp(s.directory, ".capture-*")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(file.Name()); err != nil {
			t.Fatal(err)
		}
		writeCrashBytes(t, file, "staged")
	case "appended":
		file, err := s.openBody("run", os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		writeCrashBytes(t, file, "unpublished")
	case "published":
		if err := r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("done\n")), Gap: Gap{}}); err != nil {
			t.Fatal(err)
		}
	}
}
func writeCrashBytes(t *testing.T, file *os.File, body string) {
	t.Helper()
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestAggregateCapPreservesReasonWhenNoTextFits(t *testing.T) {
	s := newStore(t, 8)
	if err := s.Append(t.Context(), "run", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	capture(t, s, native("one"), strings.Repeat("x", 40))
	values, err := s.Containers(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if values[0].Start != 8 || values[0].End != 8 || !strings.Contains(values[0].Reason, "byte limit") {
		t.Fatal(values)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != "12345678" {
		t.Fatal(string(body), err)
	}
}
func TestAttributionRefinementPreservesReceiptAndRefusesForeignIdentity(t *testing.T) {
	for _, state := range []string{"tracked", "pending", "captured"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t, 1024)
			parent := attributionState(t, s, state)
			before, err := s.Containers(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			child := parent
			child.NodeID, child.ParentNodeID, child.Step = "child", "parent", "build/leaf"
			if err = s.Track(t.Context(), "run", []ContainerLog{child}); err != nil {
				t.Fatal(err)
			}
			after, err := s.Containers(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			assertReceiptRefinement(t, before[0], after[0])
			assertForeignBindings(t, s, child)
		})
	}
}
func attributionState(t *testing.T, s *Store, state string) ContainerLog {
	t.Helper()
	parent := native("parent")
	if err := s.Track(t.Context(), "run", []ContainerLog{parent}); err != nil {
		t.Fatal(err)
	}
	if state == "tracked" {
		return parent
	}
	r, err := s.Reserve(t.Context(), "run", parent)
	if err != nil {
		t.Fatal(err)
	}
	if state == "captured" {
		if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("data\n")), Gap: Gap{}}); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}
func assertReceiptRefinement(t *testing.T, before, after ContainerLog) {
	t.Helper()
	if after.NodeID != "child" || after.Start != before.Start || after.End != before.End || after.State != before.State || after.Reason != before.Reason || after.Detail != before.Detail {
		t.Fatal("receipt changed", before, after)
	}
}
func assertForeignBindings(t *testing.T, s *Store, child ContainerLog) {
	t.Helper()
	for _, change := range []func(*ContainerLog){func(v *ContainerLog) { v.PodName = "foreign" }, func(v *ContainerLog) { v.Namespace = "foreign" }, func(v *ContainerLog) { v.NodeID = "sibling" }} {
		foreign := child
		change(&foreign)
		if err := s.Track(t.Context(), "run", []ContainerLog{foreign}); err == nil {
			t.Fatal("foreign identity accepted")
		}
	}
}
func TestConcurrentAppendsKeepExactContainerSpans(t *testing.T) {
	s := newStore(t, 1<<20)
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Go(func() {
			entry := native(fmt.Sprint(i))
			r, err := s.Reserve(t.Context(), "run", entry)
			if err != nil {
				t.Error(err)
				return
			}
			if err = s.Append(t.Context(), "run", []byte("ordinary\n")); err != nil {
				t.Error(err)
			}
			if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader(fmt.Sprintf("body %d\n", i))), Gap: Gap{}}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	for i := range 20 {
		body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{NodeID: fmt.Sprint(i)}, Filter: Filter{}})
		if err != nil || string(body) != fmt.Sprintf("body %d\n", i) {
			t.Fatal(string(body), err)
		}
	}
}
func TestStorageRefusalsAndOwnedPurge(t *testing.T) {
	s := newStore(t, 1024)
	capture(t, s, native("one"), "hello\n")
	if err := os.WriteFile(filepath.Join(s.directory, "run.plan.json"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	usage, err := s.Usage(t.Context(), "run", usageClock())
	retained := usage.Body.Logical + usage.Index.Logical
	if err != nil || retained <= 6 {
		t.Fatal(usage, err)
	}
	removed, err := s.Purge(t.Context(), "run")
	if err != nil || removed != retained {
		t.Fatal(removed, err)
	}
	if _, err = os.Stat(filepath.Join(s.directory, "run.plan.json")); err != nil {
		t.Fatal("plan removed", err)
	}
}
func TestUnsafeRunIDsAndSymlinkEvidence(t *testing.T) {
	s := newStore(t, 1024)
	for _, id := range []string{"", "../run", "/run", "a.b", strings.Repeat("x", 81)} {
		if _, err := s.Purge(t.Context(), id); err == nil {
			t.Fatal("unsafe ID", id)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.directory, "evil.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge(t.Context(), "evil"); err == nil {
		t.Fatal("symlink purged")
	}
	if usage, err := s.Usage(t.Context(), "evil", usageClock()); err != nil || usage.Body.Logical != 0 || usage.AccountingComplete {
		t.Fatal("symlink usage followed")
	}
}
func TestPublicErrorsAndCancelableStoreWait(t *testing.T) {
	s := newStore(t, 100)
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Containers(ctx, "run")
	<-s.gate
	var public *publicerr.Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &public) || public.Remedy() == "" {
		t.Fatal(err)
	}
	if strings.Contains(public.Remedy(), "check log storage") {
		t.Fatal("cancellation presented as a storage failure", public)
	}
	if err = s.Track(t.Context(), "run", []ContainerLog{{}}); err == nil {
		t.Fatal("empty binding accepted")
	}
	if _, err = Open(t.Context(), t.TempDir(), 0); err == nil {
		t.Fatal("zero run cap")
	}
	if _, _, err = s.Read(t.Context(), "run", ReadRequest{Selector: Selector{Step: "missing"}, Filter: Filter{}}); err == nil {
		t.Fatal("missing log passed")
	}
}
func TestContainerInventoryIsCopied(t *testing.T) {
	s := newStore(t, 100)
	capture(t, s, native("one"), "a\n")
	first, err := s.Containers(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	first[0].NodeID = "changed"
	second, err := s.Containers(t.Context(), "run")
	if err != nil || second[0].NodeID != "one" {
		t.Fatal("inventory aliases", err)
	}
	var zero Reservation
	if err = zero.Release(t.Context()); err == nil {
		t.Fatal("zero reservation granted release")
	}
}

func TestIndexLimitsCorruptionAndUnavailableSpans(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index []byte
	}{
		{"oversize", bytes.Repeat([]byte(" "), maxIndexBytes+1)},
		{"malformed", []byte("{")},
		{"wrong run", []byte(`{"run_id":"other","size":0}`)},
		{"invalid span", []byte(`{"run_id":"run","size":5,"containers":[{"start":4,"end":2}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, 100)
			if err := os.WriteFile(filepath.Join(s.directory, "run.index.json"), tc.index, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Containers(t.Context(), "run"); err == nil {
				t.Fatal("invalid index accepted")
			}
		})
	}
	s := newStore(t, 100)
	capture(t, s, native("one"), "body\n")
	if err := os.Truncate(filepath.Join(s.directory, "run.log"), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{NodeID: "one"}, Filter: Filter{}}); err == nil {
		t.Fatal("unavailable captured range accepted")
	}
}
func TestNilCaptureRequiresAnExplicitReason(t *testing.T) {
	s := newStore(t, 100)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{}}); err == nil {
		t.Fatal("absent source became exact empty capture")
	}
	r, err = s.Reserve(t.Context(), "other", native("two"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{Detail: "Pod is unavailable; read the native observation before retrying the run"}}); err != nil {
		t.Fatal(err)
	}
}
