package client

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

// fixtureHostKeys is a well-formed installation host-key record for tests.
const fixtureHostKeys = "[oberth.local]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB\n"

func TestHostKeysMustBeKnownHostsEntries(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"one entry", fixtureHostKeys, true},
		{"entry after a comment", "# installation\n" + fixtureHostKeys, true},
		{"empty", "", false},
		{"comment only", "# installation\n", false},
		{"not a key", "[oberth.local]:2222 ssh-ed25519 not-base64\n", false},
		{"entry then a malformed entry", fixtureHostKeys + "oberth.local not-a-key\n", false},
		{"over its share of the invitation file", strings.Repeat(fixtureHostKeys, InvitationFileBytes/2/len(fixtureHostKeys)+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ValidHostKeys(tc.text) != tc.valid {
				t.Fatalf("valid=%v for %q", !tc.valid, tc.text)
			}
		})
	}
}

func TestInviteWithoutHostKeysIssuesNothing(t *testing.T) {
	c := &Client{}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "invitation.json")
	_, err := c.Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://oberth.local"}, path, "")
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Reason() != "This profile has no SSH host keys to share." {
		t.Fatal("invitation issued without host keys", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused invitation left a file", err)
	}
}

func TestJoinRefusesAnInvitationWithoutHostKeysBeforeRedeeming(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("join redeemed an invitation without host keys")
		return nil, nil
	})}
	invitation := model.InvitationFile{Invitation: model.Invitation{Identity: "agent", ServerURL: "https://oberth.local", ExpiresAt: cfg.Time().Add(time.Hour)}, Secret: "private-handoff"}
	_, err := Join(t.Context(), cfg, invitation, filepath.Join(base, "agent"))
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Reason() != "Invitation file has no SSH host keys." || problem.Remedy() != "Ask the administrator for a new invitation from oberth invite." {
		t.Fatal("join accepted an invitation without host keys", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused join created a profile", err)
	}
}
