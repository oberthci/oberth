package runlog

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestCaptureKeepsFullDetailAcrossRestartAndRefinement(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("parent")
	detail := strings.Repeat("native message 界\n", 100) + "full ending..."
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{Detail: detail}}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), s.directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	entry.NodeID, entry.ParentNodeID = "child", "parent"
	if err = reopened.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	values, err := reopened.Containers(t.Context(), "run")
	if err != nil || len(values) != 1 || values[0].Reason != incompleteReason || values[0].Detail != detail {
		t.Fatal(values, err)
	}
	assertDetailJSON(t, values[0], detail)
}

func assertDetailJSON(t *testing.T, value ContainerLog, detail string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ContainerLog
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded.Reason != incompleteReason || decoded.Detail != detail {
		t.Fatal("JSON changed the full detail", err)
	}
}

func TestCapKeepsShortReasonAndSeparateFullDetail(t *testing.T) {
	s := newStore(t, 3)
	if err := s.Append(t.Context(), "run", []byte("abc extra")); err != nil {
		t.Fatal(err)
	}
	_, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || meta.Reason != limitReason || meta.Detail != limitDetail {
		t.Fatal(meta, err)
	}
	detail := "\n" + strings.Repeat("source detail\n", 20)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("more output")), Gap: Gap{Detail: detail}}); err != nil {
		t.Fatal(err)
	}
	values, err := s.Containers(t.Context(), "run")
	if err != nil || values[0].Reason != limitReason || values[0].Detail != detail+"\n"+limitDetail {
		t.Fatal(values, err)
	}
}

func TestInterruptedReadKeepsCallerAndSourceDetail(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(&brokenSource{}), Gap: Gap{Detail: "caller detail"}}); err != nil {
		t.Fatal(err)
	}
	values, err := s.Containers(t.Context(), "run")
	want := "caller detail\n" + interruptedDetail + "\n" + io.ErrUnexpectedEOF.Error()
	if err != nil || values[0].Reason != interruptedReason || values[0].Detail != want {
		t.Fatal(values, err)
	}
}

func TestReleaseClearsPendingReasonAndDetail(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := s.Containers(t.Context(), "run")
	if err != nil || values[0].Reason != pendingReason || values[0].Detail != pendingDetail {
		t.Fatal(values, err)
	}
	if err = r.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	values, err = s.Containers(t.Context(), "run")
	if err != nil || values[0].Reason != "" || values[0].Detail != "" {
		t.Fatal(values, err)
	}
}
