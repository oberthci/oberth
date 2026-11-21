package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestIssueListLeadsWithCountsAndContinuesBothWays(t *testing.T) {
	b := fake.Bindings()
	page := apicontract.IssuePage{Issues: []model.Issue{{ID: 51, Title: "queued work", State: model.IssueOpen, WorkStatus: model.IssueTodo}},
		Counts: model.IssueCounts{Open: 1236, Closed: 480, Todo: 900, Active: 12, Waiting: 4, Review: 320, Stale: 77}, Total: 1236, Offset: 50, NextBefore: 51, NextAfter: 51}
	list := &fake.Handler[apicontract.IssueListArgs, apicontract.IssuePage]{Value: page}
	b.IssueList.Handler = list
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"issue", "list", "--after", "101", "--stale"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if lines[0] != "1236 open (900 todo, 12 active, 4 waiting, 320 review, 77 stale), 480 closed" || lines[1] != "Showing 51-51 of 1236." {
		t.Fatalf("count summary is not first:\n%s", out.String())
	}
	// Commands stand on lines of their own, whole and copyable.
	if strings.Join(lines[len(lines)-4:], "|") != "Older page:|oberth issue list --before 51|Newer page:|oberth issue list --after 51" || !strings.Contains(out.String(), "issues[0] / title: queued work") {
		t.Fatalf("page or continuations missing:\n%s", out.String())
	}
	inputs, _, _ := list.Observed()
	if len(inputs) != 1 || inputs[0].After != 101 || !inputs[0].Stale {
		t.Fatalf("flags did not reach issue_list: %+v", inputs)
	}
}
