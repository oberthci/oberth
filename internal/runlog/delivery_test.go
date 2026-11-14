package runlog

import (
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
