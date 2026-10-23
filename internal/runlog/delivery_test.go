package runlog

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestDeliveryRecordsDoNotBelongToAnyStep(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("delivery")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// A real step named oberth/log is legal and must remain ordinary output.
	body := "[oberth/log] real step\n" + runprogress.LogDeliveryPrefix + "{}\n[oberth/log] next line\n" + runprogress.LogDeliveryPrefix + "{}\n"
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	want := "[oberth/log] real step\n[oberth/log] next line\n"
	active, err := store.ReadActive("delivery", "oberth", "log")
	if err != nil || string(active) != want {
		t.Fatalf("active=%q err=%v", active, err)
	}
	if _, err := store.BuildIndex("delivery"); err != nil {
		t.Fatal(err)
	}
	retained, err := store.Read("delivery", "oberth", "log")
	if err != nil || string(retained) != want {
		t.Fatalf("retained=%q err=%v", retained, err)
	}
}

func TestInitialLiveTailStartsAtAnAttributedRecord(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("tail")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// The tail window lands on a bracketed name in the middle of another
	// step's output. Returning that suffix would falsely attribute the line.
	last := "[build/two] " + strings.Repeat("x", 70<<10) + "[build/one] command text\n"
	if _, err := file.WriteString("[setup/go] earlier\n" + last); err != nil {
		t.Fatal(err)
	}
	for _, bound := range []int64{int64(len("[build/one] command text\n")), int64(len(last)), int64(len(last) + 1)} {
		chunk, next, size, err := store.ReadFrom("tail", -1, bound)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(chunk), "[build/two] ") && !strings.HasPrefix(string(chunk), "[setup/go] ") {
			t.Fatalf("tail started in a record: %.50q", chunk)
		}
		if int64(len(chunk)) > bound || next > size {
			t.Fatal("tail exceeded its byte/cursor bounds")
		}
		// The returned cursor must still continue exactly where the chunk ends.
		continuation, _, _, err := store.ReadFrom("tail", next, int64(len(last)))
		if err != nil || !strings.HasSuffix(string(chunk)+string(continuation), last) {
			t.Fatalf("initial tail/cursor lost or duplicated output: %v", err)
		}
	}
}
