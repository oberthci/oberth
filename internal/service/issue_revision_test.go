package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestIssueUpdateRevisionSerializesConcurrentWriters(t *testing.T) {
	f := newControlFixture(t)
	issue, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			args, _ := json.Marshal(map[string]any{"id": issue.ID, "title": fmt.Sprintf("title-%d", i), "expected_revision": issue.Revision})
			<-start
			_, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, "issue_update", args)
			done <- err
		}(i)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-done
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrInvalidState) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	got, err := f.store.Issue(t.Context(), issue.ID)
	if err != nil || got.Revision != 2 || got.Body != "original" {
		t.Fatalf("post-update issue: %+v %v", got, err)
	}
	listed, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, "issue_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.(api.IssueListResponse).Issues
	if len(rows) != 1 || rows[0].Revision != got.Revision {
		t.Fatalf("issue list lost revision: %+v", rows)
	}
	for _, raw := range []string{`{"id":1,"title":"invalid","expected_revision":null}`, `{"id":1,"title":"invalid","expected_revision":0}`, `{"id":1,"title":"invalid","expected_revision":1.5}`} {
		if _, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, "issue_update", json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid precondition %s: %v", raw, err)
		}
	}
}
