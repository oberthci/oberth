//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestJoinBindingRefusalCanRetrySameInvitation(t *testing.T) {
	command, invitation, redemptions := joinRetryFixture(t)
	setting := "url.ssh://git@rewrite.oberth.ci/.insteadOf"
	if _, err := runGit(t.Context(), command.app.directory, "config", "--local", setting, "file:///"); err != nil {
		t.Fatal(err)
	}
	before, err := client.ReadPrivate(invitation)
	if err != nil {
		t.Fatal(err)
	}
	if code := command.app.run(t.Context(), []string{"join", invitation}); code != 1 {
		t.Fatal("join accepted an incompatible binding", code)
	}
	message := strings.Join(strings.Fields(command.diagnostic.String()), " ")
	if !strings.Contains(message, "Git rewrites the Oberth fetch address.") || !strings.Contains(message, "then rerun join.") {
		t.Fatal("join refusal lost recovery guidance", command.diagnostic.String())
	}
	refusedRedemptions := redemptions.Load()
	after, err := client.ReadPrivate(invitation)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused join changed invitation", err)
	}
	if _, err := runGit(t.Context(), command.app.directory, "config", "--local", "--unset-all", setting); err != nil {
		t.Fatal(err)
	}
	command.diagnostic.Reset()
	if code := command.app.run(t.Context(), []string{"join", invitation}); code != 0 {
		t.Fatal("same invitation could not recover after fixing Git", code, command.diagnostic.String())
	}
	if refusedRedemptions != 0 || redemptions.Load() != 1 {
		t.Fatal("refused join spent an invitation", refusedRedemptions, redemptions.Load())
	}
	assertInvitationRetryState(t, command, invitation)
}

func assertInvitationRetryState(t *testing.T, command repoAddCommand, invitation string) {
	t.Helper()
	if _, err := os.Lstat(invitation); !os.IsNotExist(err) {
		t.Fatal("successful join kept the spent invitation", err)
	}
	address, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth")
	if err != nil || address != gitBindingAddress("origin/team/widget") {
		t.Fatal("retry did not bind the checkout", address, err)
	}
}

func joinRetryFixture(t *testing.T) (repoAddCommand, string, *atomic.Int32) {
	t.Helper()
	f, command, _, _ := joinedCheckoutFixture(t)
	if code := command.run(t); code != 0 {
		t.Fatal("fixture checkout was not bound", command.diagnostic.String())
	}
	command.app.base = t.TempDir()
	if err := os.Chmod(command.app.base, 0700); err != nil {
		t.Fatal(err)
	}
	invitation := filepath.Join(command.app.base, "invitation.json")
	raw, err := json.Marshal(f.invitation(t, "retry-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WritePrivate(invitation, raw); err != nil {
		t.Fatal(err)
	}
	original := f.client(f.config.Listen.HTTPS).Transport
	redemptions := new(atomic.Int32)
	command.app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/enroll") {
			redemptions.Add(1)
		}
		return original.RoundTrip(r)
	})
	return command, invitation, redemptions
}
