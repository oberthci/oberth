package model

import (
	"encoding/json"
	"testing"
)

// A resumed run is never evidence, even without the Assisted mark, and an
// ordinary origin keeps its wire shape.
func TestResumedRunHasAssistance(t *testing.T) {
	t.Parallel()
	if (RunOrigin{}).HasAssistance() || (RunOrigin{}).Resumed() {
		t.Fatal("ordinary origin assisted")
	}
	o := RunOrigin{ResumedFrom: "run-1"}
	if !o.Resumed() || !o.HasAssistance() {
		t.Fatal("resumed origin is evidence", o)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var back RunOrigin
	if err := json.Unmarshal(raw, &back); err != nil || back.ResumedFrom != "run-1" {
		t.Fatal("resumed_from lost", string(raw), err)
	}
}
