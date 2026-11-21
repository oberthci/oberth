package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRefusedInviteRemovesHandoffAndAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Run: func(context.Context) (model.Invitation, error) {
		attempts++
		if attempts == 1 {
			return model.Invitation{}, publicerr.New(publicerr.InvalidInput, "Invitation address must match the public address.", "Correct server_url in the selected profile's installation.json, then run oberth invite <upstream>.", nil)
		}
		return model.Invitation{Identity: "agent", ServerURL: "https://oberth.local", ExpiresAt: fake.NewClock().Now().Add(time.Hour)}, nil
	}}
	b := fake.Bindings()
	b.UplinkInvite.Handler = h
	c := testClient(t, handlerTransport(server(t, b)), nil)
	defer c.Close()
	args := apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://oberth.local"}
	if _, err := c.Invite(t.Context(), args, path, fixtureHostKeys); err == nil {
		t.Fatal("invitation was not refused")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("refused invitation retained its handoff", err)
	}
	if _, err := c.Invite(t.Context(), args, path, fixtureHostKeys); err != nil || attempts != 2 {
		t.Fatal("retry did not succeed", err, attempts)
	}
	data, err := ReadPrivate(path)
	var handoff model.InvitationFile
	if err != nil || json.Unmarshal(data, &handoff) != nil || handoff.ExpiresAt.IsZero() || handoff.Secret == "" {
		t.Fatal("successful retry lost handoff")
	}
}

func TestUnknownInviteOutcomeKeepsHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { attempts++; return nil, io.ErrUnexpectedEOF }), nil)
	defer c.Close()
	_, err := c.Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://oberth.local"}, path, fixtureHostKeys)
	data, readErr := ReadPrivate(path)
	var handoff model.InvitationFile
	if !IsOutcomeUnknown(err) || readErr != nil || json.Unmarshal(data, &handoff) != nil || handoff.Secret == "" || attempts != 1 {
		t.Fatal("unknown invitation outcome lost its handoff or replayed")
	}
}
