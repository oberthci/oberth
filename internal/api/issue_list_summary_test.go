package api

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestClosedIssueListOmitsWorkMetadata(t *testing.T) {
	for _, state := range []model.IssueState{model.IssueOpen, model.IssueClosed} {
		body, err := json.Marshal(IssueListSummary{ID: 42, State: state, WorkStatus: model.IssueWorkStatus("active"), Stale: false})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"work_status", "stale"} {
			_, exists := fields[key]
			if exists != (state == model.IssueOpen) {
				t.Fatal(string(body))
			}
		}
		if string(fields["ID"]) != "42" {
			t.Fatal(string(body))
		}
	}
}
