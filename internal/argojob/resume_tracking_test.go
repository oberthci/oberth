package argojob

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"testing"
)

func TestResumeStaleTrackerCannotUntrackOwner(t *testing.T) {
	t.Parallel()
	e := New(Config{})
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	first := nativebinding.RunBinding{RunID: "r1", Workflow: nativebinding.Object{Name: "shared", UID: "uid"}}
	e.track(first, admissioncontract.Plan{}, trackingSeed{})
	old := e.tracked["shared"]
	second := first
	second.RunID = "r2"
	e.track(second, admissioncontract.Plan{}, trackingSeed{})
	e.retire(old)
	if current := e.tracked["shared"]; current == nil || current.binding.RunID != "r2" {
		t.Fatal("stale predecessor retired resumed owner")
	}
}
