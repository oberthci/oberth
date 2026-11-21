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
	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedPendingReceiveCannotStopStartup(t *testing.T) {
	for _, shape := range []string{"revoked", "unregistered", "corrupt", "corrupt-orphan"} {
		t.Run(shape, func(t *testing.T) {
			f := composeFixture(t, composeBinary(t))
			f.prepareCommit(t)
			cancel, done := f.run(t)
			f.checkStarting(t, done)
			f.waitStarted(t, done)
			f.bindClient(t)
			pending := f.leavePendingReceive(t)
			if shape == "revoked" {
				if err := f.owners.c.store.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, pending.Event.Fingerprint); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if shape != "revoked" {
				pending = f.alterStartupReceive(t, shape, pending)
			}
			f.restartChannels(t)
			stop, restarted := f.run(t)
			defer stop()
			f.checkStarting(t, restarted)
			f.waitStarted(t, restarted)
			if strings.HasPrefix(shape, "corrupt") {
				f.assertStartupOutbox(t, pending.Event.Repository)
			} else {
				f.assertReceiveAbandoned(t, pending)
			}
		})
	}
}

func (f *composedFixture) alterStartupReceive(t *testing.T, shape string, r gitcache.Reservation) gitcache.Reservation {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	if shape == "unregistered" || shape == "corrupt-orphan" {
		next := filepath.Join(filepath.Dir(path), "orphan.git")
		if err := os.Rename(path, next); err != nil {
			t.Fatal(err)
		}
		path, r.Event.Repository = next, "origin/team/orphan"
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(shape, "corrupt") {
		data = []byte("{damaged")
	}
	if err := os.WriteFile(filepath.Join(path, "oberth-receive.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *composedFixture) assertStartupOutbox(t *testing.T, repo string) {
	t.Helper()
	a := serverAdministration{f.owners}
	out, err := a.ReceiveOutbox(t.Context(), stateport.Action{Actor: "operator"}, nil, apicontract.ReceiveOutboxArgs{Action: "list"})
	if err != nil || len(out.Records) != 1 || out.Records[0].Repo != repo || out.Records[0].DisplayReason == "receive pending" {
		t.Fatal("missing startup repair record", out, err)
	}
	f.assertStartupReceiveStatus(t, repo)
	if repo != gittest.Repository {
		if err := f.owners.cache.Init(t.Context(), gittest.Repository); err != nil {
			t.Fatal("orphaned receive blocked unrelated repository", err)
		}
	}
	row := out.Records[0]
	_, err = a.ReceiveOutbox(t.Context(), stateport.Action{Actor: "operator", Reason: "discard damaged receive"}, nil, apicontract.ReceiveOutboxArgs{Action: "abandon", ID: row.ID})
	if err != nil {
		t.Fatal("operator cannot clear receive", err)
	}
	if _, err := f.owners.cache.PendingReceive(t.Context(), repo); !os.IsNotExist(err) {
		t.Fatal("abandon left record", err)
	}
	if err := f.owners.cache.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("repaired repository blocked", err)
	}
}

func (f *composedFixture) assertStartupReceiveStatus(t *testing.T, repo string) {
	t.Helper()
	a := serverAdministration{f.owners}
	status, err := a.Status(t.Context(), "")
	if err != nil || !status.Ready {
		t.Fatal("pending receive stopped server readiness", status, err)
	}
	found := false
	for _, part := range status.Parts {
		if part.Name == "pending pushes" && strings.Contains(part.DisplayReason, "set aside") {
			found = true
		}
	}
	if !found {
		t.Fatal("system status hid set-aside receive", status)
	}
	if text := f.trace.String(); strings.Count(text, `msg="receive set aside"`) != 1 || !strings.Contains(text, "repository="+repo) {
		t.Fatal("startup must log one line per set-aside record", text)
	}
}
