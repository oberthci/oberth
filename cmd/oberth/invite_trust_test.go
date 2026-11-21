package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/strictjson"
)

// invite checks the record it copies at issue time: it must pin the
// installation's SSH endpoint and fit the bound join reads, or no invitation
// is created and no file is written.
func TestInviteIssuesOnlyAHostKeyRecordJoinAccepts(t *testing.T) {
	atBound := strings.Repeat(issuerHostKeys, client.InvitationFileBytes/2/len(issuerHostKeys))
	for _, tc := range []struct {
		name, record string
		issued       bool
	}{
		{"pins the endpoint", issuerHostKeys, true},
		{"at the bound", atBound, true},
		{"another host only", strings.Replace(issuerHostKeys, "[127.0.0.1]", "[127.0.0.2]", 1), false},
		{"over the bound", atBound + issuerHostKeys, false},
		{"not a key record", "127.0.0.1 not-a-key\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.Bindings()
			h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Value: model.Invitation{ID: "invitation", Identity: "agent", ServerURL: "https://127.0.0.1", CAPin: strings.Repeat("a", 64), Scope: []string{"source"}}}
			b.UplinkInvite.Handler = h
			app, _, diagnostic := fixture(t, b)
			env, key := app.env, issuerKey(t, issuerSSH, tc.record)
			app.env = func(name string) string {
				if name == "OBERTH_CLIENT_KEY" {
					return key
				}
				return env(name)
			}
			path := filepath.Join(decidingDirectory(t), "invitation.json")
			code := app.run(t.Context(), []string{"invite", "source", "--out", path})
			_, _, calls := h.Observed()
			if !tc.issued {
				_, statErr := os.Lstat(path)
				if code != 1 || calls != 0 || !errors.Is(statErr, os.ErrNotExist) || !strings.HasPrefix(diagnostic.String(), "The profile's SSH trust does not pin this installation.\nRun oberth invite with the administrator profile that install saved.\n") {
					t.Fatal("unusable record was issued", code, calls, statErr, diagnostic.String())
				}
				return
			}
			if code != 0 || calls != 1 {
				t.Fatal("usable record was refused", code, diagnostic.String())
			}
			assertJoinReads(t, path, tc.record)
		})
	}
}

func assertJoinReads(t *testing.T, path, record string) {
	t.Helper()
	data, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err = strictjson.DecodeStrictObject(data, &invitation, client.InvitationFileBytes); err != nil || invitation.KnownHosts != record || !client.ValidHostKeys(invitation.KnownHosts) {
		t.Fatal("join would not read the issued file", len(data), err)
	}
}
