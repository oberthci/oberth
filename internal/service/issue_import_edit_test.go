package service_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportEditBudgetIsAtomicAndComplete(t *testing.T) {
	for _, real := range []bool{false, true} {
		for _, appendText := range []bool{false, true} {
			t.Run(fmt.Sprint(real, appendText), func(t *testing.T) {
				p := people(t)
				if !real {
					p.f.c.People.Issues = p.f.store.native
					p.f.restart(t)
				}
				checkImportEditBudget(t, p, appendText)
			})
		}
	}
}

func checkImportEditBudget(t *testing.T, p *peopleFixture, appendText bool) {
	t.Helper()

	v := importRecord(42)
	v.History[0].Text = strings.Repeat("h", 120000)
	if _, err := importCall(t, p, importArgs(t, v)); err != nil {
		t.Fatal(err)
	}
	before := readImportedIssue(t, p, 42)
	projected := before.Clone()
	projected.Revision++
	projected.UpdatedAt = p.f.c.Now().UTC()
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	room := model.IssueBytes - len(raw)
	if room <= 0 {
		t.Fatal("fixture has no edit room")
	}
	for _, extra := range []int{1, 0} {
		text := strings.Repeat("x", room+extra)
		err = editImportedIssue(t, p, before.ID, importEdit{text, appendText})
		if extra == 1 {
			if err == nil {
				t.Fatal("oversized edit committed")
			}
			if got := readImportedIssue(t, p, 42); !reflect.DeepEqual(before, got) {
				t.Fatal("refused edit changed issue")
			}
		} else if err != nil {
			t.Fatal("exact budget edit refused", err)
		}
	}
	got := readImportedIssue(t, p, 42)
	if !reflect.DeepEqual(got.History, before.History) || got.Imported == nil {
		t.Fatal("source history trimmed")
	}
}

func readImportedIssue(t *testing.T, p *peopleFixture, id int64) model.Issue {
	t.Helper()
	call, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: id})
	v, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type importEdit struct {
	text       string
	appendText bool
}

func editImportedIssue(t *testing.T, p *peopleFixture, id int64, edit importEdit) error {
	t.Helper()
	if edit.appendText {
		call, err := p.f.s.PrepareIssueAppend(t.Context(), adminCaller(), apicontract.IssueAppendArgs{ID: id, Text: edit.text})
		_, err = execute(t.Context(), call, err)
		return err
	}
	call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: id, Title: new(importRecord(id).Title + edit.text)})
	_, err = execute(t.Context(), call, err)
	return err
}

func TestIssueImportUpdateChecksLatestRetainedFields(t *testing.T) {
	p := people(t)
	v := importRecord(42)
	if _, err := importCall(t, p, importArgs(t, v)); err != nil {
		t.Fatal(err)
	}
	prepared, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: 42, Title: new(strings.Repeat("t", 70000))})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("b", 70000)
	if _, err = p.f.c.People.Issues.UpdateIssue(t.Context(), stateport.Action{Actor: admin().Identity, Caller: admin()}, model.IssuePatch{ID: 42, Body: &body}); err != nil {
		t.Fatal(err)
	}
	before := readImportedIssue(t, p, 42)
	if _, err = prepared(t.Context()); err == nil {
		t.Fatal("stale preflight bypassed complete budget")
	}
	if got := readImportedIssue(t, p, 42); !reflect.DeepEqual(before, got) {
		t.Fatal("failed stale edit changed issue")
	}
}
