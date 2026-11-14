package runlog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
)

func testContainerLog(node, pod, name string, body []byte) ContainerLog {
	return ContainerLog{NodeID: node, PodUID: pod, PodName: "pod-" + pod, Namespace: "pipeline", ContainerName: name, Reader: bytes.NewReader(body)}
}

func TestContainerLogCopyIsOnceAndHasExactEmptyOffsets(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("run")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := store.AppendRunLog("run", []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	first := testContainerLog("node-a", "pod-a", "main", []byte("body\n"))
	claimed, err := store.ReserveContainerLog("run", first)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if err := store.StoreContainerLog("run", first, 64); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreContainerLog("run", first, 64); err != nil {
		t.Fatal(err)
	}
	empty := testContainerLog("node-b", "pod-b", "main", nil)
	if err := store.StoreContainerLog("run", empty, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendRunLog("run", []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	index, err := store.BuildIndex("run")
	if err != nil || len(index.Containers) != 2 {
		t.Fatal(index, err)
	}
	if got := index.Containers[1]; got.Start != 12 || got.End != 12 {
		t.Fatal("empty copy lost append position", got)
	}
	raw, _, err := store.ReadRunLog(t.Context(), "run", Filter{})
	if err != nil || string(raw) != "before\nbody\nafter\n" {
		t.Fatal(string(raw), err)
	}
	body, _, _, err := store.ReadContainerLog(t.Context(), "run", "node-a", "main", Filter{})
	if err != nil || string(body) != "body\n" {
		t.Fatal(string(body), err)
	}
}

func TestContainerLogLimitIsAggregateAndTruncationExplainsItself(t *testing.T) {
	store, err := Open(t.TempDir(), WithRunLimitBytes(128))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		entry := testContainerLog(fmt.Sprint(i), fmt.Sprint(i), "main", bytes.Repeat([]byte("a"), 256))
		if err := store.StoreContainerLog("run", entry, 128); err != nil {
			t.Fatal(err)
		}
		body, _, saved, err := store.ReadContainerLog(t.Context(), "run", entry.NodeID, "main", Filter{})
		if err != nil || len(body) > 128 || !bytes.Contains(body, []byte("run log limit reached")) || saved.Reason == "" {
			t.Fatal(len(body), saved, err)
		}
	}
	if _, err := store.AppendRunLog("run", bytes.Repeat([]byte("late observation"), 30)); err != nil {
		t.Fatal(err)
	}
	index, err := store.BuildIndex("run")
	if err != nil || index.Size != 128 {
		t.Fatal(index, err)
	}
}

func TestContainerLogReservationSurvivesRestartAndPreservesMarkers(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	entry := testContainerLog("node", "pod", "main", nil)
	if err := store.TrackContainerLogs("run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ReserveContainerLog("run", entry); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := reopened.ReserveContainerLog("run", entry); err != nil || claimed {
		t.Fatal("interrupted capture read twice", claimed, err)
	}
	body, _, _, err := reopened.ReadContainerLog(context.Background(), "run", "node", "main", Filter{})
	if err != nil || !bytes.Contains(body, []byte("copy has not completed")) {
		t.Fatal(string(body), err)
	}
	historical := []byte("@@oberth-step-progress@@ " + `{"version":1,"burn":"test","step":"one","ordinal":0,"status":"queued"}` + "\n" + "plain\n")
	if _, err := store.AppendRunLog("old-run", historical); err != nil {
		t.Fatal(err)
	}
	body, _, err = store.ReadRunLog(t.Context(), "old-run", Filter{})
	if err != nil || !bytes.Equal(body, historical) {
		t.Fatal(string(body), err)
	}
}

func TestContainerLogConcurrentAppendsKeepSpansAttributable(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for i := range 20 {
		writers.Go(func() {
			entry := testContainerLog(fmt.Sprint(i), fmt.Sprint(i), "main", []byte(fmt.Sprintf("container-%d\n", i)))
			if _, err := store.AppendRunLog("run", []byte("system\n")); err != nil {
				t.Error(err)
			}
			if err := store.StoreContainerLog("run", entry, 4096); err != nil {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	for i := range 20 {
		body, _, _, err := store.ReadContainerLog(t.Context(), "run", fmt.Sprint(i), "main", Filter{})
		if err != nil || string(body) != fmt.Sprintf("container-%d\n", i) {
			t.Fatal(string(body), err)
		}
	}
	f, err := store.openRunLog("run", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil || bytes.Count(raw, []byte("system\n")) != 20 {
		t.Fatal(string(raw), err)
	}
}
