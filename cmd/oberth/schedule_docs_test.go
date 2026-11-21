package main

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"strings"
	"testing"
)

func TestScheduleListDocumentationMatchesObservedFields(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	data, _ := json.Marshal(apicontract.SchedulePage{Schedules: []apicontract.Schedule{{Repo: "origin/team/widget", Entry: "nightly", Paused: true, DisplayReason: "schedule branch not found"}}})
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"origin/team/widget", "entry: nightly", "paused: true", "schedule branch not found"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("schedule fact missing", text, out.String())
		}
	}
	if strings.Contains(out.String(), "next") {
		t.Fatal("list invented a firing time", out.String())
	}
	doc, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	if !strings.Contains(text, "repository, pause state and any refusal") || !strings.Contains(text, "The list does not predict the next occurrence.") || strings.Contains(text, "refusal or next occurrence") {
		t.Fatal("guide promises a value the list does not carry")
	}
}
