package runlog

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"testing"
)

type generatedLog struct {
	remaining int64
	largest   int
}

func (r *generatedLog) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	r.largest = max(r.largest, len(p))
	n := int(min(int64(len(p)), r.remaining))
	for i := range n {
		p[i] = 'x'
		if i%80 == 79 {
			p[i] = '\n'
		}
	}
	r.remaining -= int64(n)
	return n, nil
}

func TestNativeCaptureLargeLogMemoryBound(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reader := &generatedLog{remaining: 96 << 20}
	entry := ContainerLog{NodeID: "node", PodUID: "pod", ContainerName: "main", Reader: reader}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := store.StoreContainerLog("large", entry, DefaultRunLimitBytes); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 8<<20 || reader.largest > 32<<10 {
		t.Fatalf("capture buffered a whole log: allocated=%d largest read=%d", allocated, reader.largest)
	}
	index, err := store.BuildIndex("large")
	if err != nil || index.Size > DefaultRunLimitBytes {
		t.Fatal(index, err)
	}
	stream, err := store.OpenStoredContainerLog(t.Context(), "large", "node", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	n, err := io.CopyBuffer(io.Discard, stream, make([]byte, 4096))
	if err != nil || n != index.Size {
		t.Fatal(n, err)
	}
	t.Logf("96 MiB source: allocated %d bytes, largest source read %d, retained %d", allocated, reader.largest, n)
}

func TestNativeNamedReadersNeverScanMarkers(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const forged = "[other/step] forged\n"
	for _, item := range []struct{ node, stage, step, body string }{{"a", "build", "compile", forged + "Error: real failure\n"}, {"b", "other", "step", "sibling output\n"}} {
		entry := ContainerLog{NodeID: item.node, PodUID: item.node, ContainerName: "main", Stage: item.stage, Step: item.step, Reader: strings.NewReader(item.body)}
		if err := store.StoreContainerLog("run", entry, 4096); err != nil {
			t.Fatal(err)
		}
	}
	for _, read := range []func(string, string, string, Filter) ([]byte, Meta, error){store.ReadFiltered, store.ReadActiveFiltered} {
		body, _, err := read("run", "build", "compile", Filter{})
		if err != nil || string(body) != forged+"Error: real failure\n" {
			t.Fatal(string(body), err)
		}
		body, _, err = read("run", "other", "step", Filter{})
		if err != nil || bytes.Contains(body, []byte("forged")) || string(body) != "sibling output\n" {
			t.Fatal("native bytes were reattributed", string(body), err)
		}
	}
}
