package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestIssueHTTPListOmitsBodiesAndPrivateFields(t *testing.T) {
	f := newControlFixture(t)
	_, err := f.store.CreateManualIssue(t.Context(), "reader", model.ManualIssueSpec{Title: "work", Body: "private long body"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.api(t).Issues(t.Context(), api.Actor{Identity: "reader"}, api.IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	for _, hidden := range []string{"private long body", "ScopeUpstreamID", "CIWorkSequence"} {
		if strings.Contains(string(raw), hidden) {
			t.Errorf("list leaks %s: %s", hidden, raw)
		}
	}
	if !strings.Contains(string(raw), "work") {
		t.Fatalf("list missing metadata: %s", raw)
	}
}
