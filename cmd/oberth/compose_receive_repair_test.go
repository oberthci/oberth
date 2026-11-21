//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestComposedReceiveRepairGuidanceThroughAPI(t *testing.T) {
	for _, shape := range []string{"outside-writer", "unprepared-parent", "blocked-rollback"} {
		t.Run(shape, func(t *testing.T) {
			f := composeFixture(t, composeBinary(t))
			f.prepareCommit(t)
			f.prepareAdministrator(t)
			cancel, done := f.run(t)
			defer cancel()
			f.checkStarting(t, done)
			f.waitStarted(t, done)
			f.bindClient(t)
			pending := f.leavePendingReceive(t)
			f.damagePendingReceive(t, shape, pending)
			reason := "receive refs need manual repair"
			remedy := "Have the operator repair storage or restore the pending receive's before refs manually; then retry or abandon."
			if shape == "blocked-rollback" {
				reason = "push refused; rollback is blocked"
				remedy = "Ask the server operator to fix repository storage, then retry this push."
			}
			f.assertReceiveRepairAPI(t, pending, reason, remedy)
		})
	}
}

func (f *composedFixture) damagePendingReceive(t *testing.T, shape string, pending gitcache.Reservation) {
	t.Helper()
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	ref := pending.Event.Updates[0].Ref
	switch shape {
	case "outside-writer":
		f.git.Git(t, store, "update-ref", ref, f.git.Commit)
	case "unprepared-parent":
		next := pending.Event.Updates[0].NewSHA
		f.git.Git(t, store, "update-ref", "refs/heads/topic/leaf", next)
		pending.Before["refs/heads/topic"] = f.git.Commit
		pending.Event.Updates = []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.git.Commit}, {Ref: "refs/heads/topic/leaf", NewSHA: next}}
		pending.Prepared = map[string]struct{}{"refs/heads/topic/leaf": {}}
	case "blocked-rollback":
		pending.State = "rejected"
		if err := os.WriteFile(filepath.Join(store, ref+".lock"), []byte("foreign writer"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store, "oberth-receive.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *composedFixture) assertReceiveRepairAPI(t *testing.T, pending gitcache.Reservation, reason, remedy string) {
	t.Helper()
	remote := f.administratorClient(t)
	definition := receiveRepairDefinition(t)
	for _, action := range []string{"retry", "abandon"} {
		input, err := json.Marshal(apicontract.ReceiveOutboxArgs{Action: action, ID: pending.Event.ID, Repo: gittest.Repository, Reason: "repair probe"})
		if err != nil {
			t.Fatal(err)
		}
		data, refused, err := remote.Call(t.Context(), definition, input)
		if err != nil || !refused {
			t.Fatalf("%s lost operator guidance: %s, refused=%v, err=%v", action, data, refused, err)
		}
		var got struct{ Code, Reason, Remedy, Detail string }
		if err = json.Unmarshal(data, &got); err != nil || got.Code != "unavailable" || got.Reason != reason || got.Remedy != remedy || got.Detail != "" {
			t.Fatal("API did not return the exact repair guidance", string(data), err)
		}
		if strings.Contains(string(data), filepath.Dir(f.config.DatabaseFile)) || strings.Contains(string(data), ".lock") {
			t.Fatal("API leaked storage details", string(data))
		}
	}
}

func receiveRepairDefinition(t *testing.T) apicontract.Definition {
	t.Helper()
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name == "receive_outbox" {
			return d
		}
	}
	t.Fatal("receive_outbox definition missing")
	return apicontract.Definition{}
}
