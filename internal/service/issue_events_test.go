package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"testing"
)

func TestManualIssueChangesEmitIssueUpdated(t *testing.T) {
	t.Parallel()
	p := people(t)
	p.f.c.Events = p.state
	p.f.restart(t)
	issue := createTask(t, p, "origin", "body")
	call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: issue.ID, Title: new("edited")})
	if _, err = execute(t.Context(), call, err); err != nil {
		t.Fatal(err)
	}
	wait, err := p.f.s.PrepareEventsWait(t.Context(), adminCaller(), apicontract.EventsWaitArgs{AfterID: new(int64(0)), Kinds: []string{"issue.updated"}, TimeoutS: 1})
	page, err := execute(t.Context(), wait, err)
	if err != nil || page.TimedOut || len(page.Events) != 2 {
		t.Fatal("issue changes did not reach waiter", page, err)
	}
	for _, e := range page.Events {
		if e.Kind != string(model.EventIssueUpdated) || e.RunID != "" || e.Summary != "issue updated" {
			t.Fatal("issue announced as another fact", e)
		}
	}
}
