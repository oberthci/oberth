//go:build linux

package main

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func TestReviewNativeRefusalMustKeepKnownFile(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.git.Write(t, "next.txt", []byte("next\n"))
	f.git.CommitFile(t, "next")
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	const file = ".oberth/selected.yaml"
	if output, err := f.pushResult(t, "refs/heads/main", "-o", "oberth.run=1", "-o", "oberth.file="+file); err == nil {
		t.Fatal("expected refusal", output)
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != 1 {
		t.Fatal("missing refusal", len(rows), err)
	}
	var detail store.ReceiveRefusal
	if err := json.Unmarshal([]byte(rows[0].Details), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.File != file {
		t.Fatalf("native refusal lost known file: got=%q, want=%q, details=%s", detail.File, file, rows[0].Details)
	}
	event := gitcache.ReceiveEvent{Actor: rows[0].Actor, Repository: rows[0].ResourceID, Options: gitcache.PushOptions{File: file}}
	f.assertRefusalFilePreference(t, event)
}

func (f *composedFixture) assertRefusalFilePreference(t *testing.T, event gitcache.ReceiveEvent) {
	t.Helper()
	for _, specific := range []string{"", ".oberth/specific.yaml"} {
		if err := f.owners.refusedReceive(t.Context(), event, &app.Refusal{Code: "grant", File: specific}); err != nil {
			t.Fatal(err)
		}
		rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
		if err != nil || len(rows) == 0 {
			t.Fatal("missing application refusal", rows, err)
		}
		var detail store.ReceiveRefusal
		if err := json.Unmarshal([]byte(rows[0].Details), &detail); err != nil {
			t.Fatal(err)
		}
		want := event.Options.File
		if specific != "" {
			want = specific
		}
		if detail.File != want {
			t.Fatal("application refusal lost its specific file or event fallback", detail.File, want)
		}
	}
}
