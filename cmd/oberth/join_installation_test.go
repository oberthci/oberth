package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

func TestJoinRefusesAnotherInstallationBeforeRedemption(t *testing.T) {
	for _, mismatch := range []string{"address", "authority", "missing-binding"} {
		t.Run(mismatch, func(t *testing.T) {
			checkJoinInstallationMismatch(t, mismatch)
		})
	}
}

func TestThirdJoinKeepsSeparateProfiles(t *testing.T) {
	s := newJoinScenario(t, "install")
	raw, err := client.ReadPrivate(s.invitation)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oberth.local", "oberth.local-uplink", "oberth.local-uplink-2"} {
		if err = client.WritePrivate(s.invitation, raw); err != nil {
			t.Fatal(err)
		}
		if code := s.app.run(t.Context(), []string{"join", s.invitation}); code != 0 {
			t.Fatal("repeat join failed", name, s.diagnostic.String())
		}
		bound, err := runGit(t.Context(), s.app.directory, "config", "--get", "oberth.profile")
		if err != nil || bound != filepath.Join(s.app.base, name) {
			t.Fatal("wrong profile selected", bound, name, err)
		}
	}
	if err = client.WritePrivate(s.invitation, raw); err != nil {
		t.Fatal(err)
	}
	s.app.env = func(key string) string {
		if key == "OBERTH_PROFILE" {
			return "oberth.local"
		}
		return ""
	}
	s.diagnostic.Reset()
	if s.app.run(t.Context(), []string{"join", s.invitation}) != 1 || !strings.Contains(s.diagnostic.String(), "OBERTH_PROFILE to a new name") {
		t.Fatal("explicit profile collision was not explained", s.diagnostic.String())
	}
}

func checkJoinInstallationMismatch(t *testing.T, mismatch string) {
	t.Helper()
	s := newJoinScenario(t, "install")
	raw, err := client.ReadPrivate(s.invitation)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err = json.Unmarshal(raw, &invitation); err != nil {
		t.Fatal(err)
	}
	switch mismatch {
	case "address":
		invitation.ServerURL = "https://other.oberth.ci"
	case "authority":
		invitation.CAPin = strings.Repeat("0", len(invitation.CAPin))
	case "missing-binding":
		if _, err = runGit(t.Context(), s.app.directory, "config", "--unset", "oberth.profile"); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ = json.Marshal(invitation)
	if err = client.WritePrivate(s.invitation, raw); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(s.app.directory, ".git", "config")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedJoinState(t, s, raw, before)
}

func assertRefusedJoinState(t *testing.T, s *joinScenario, raw, before []byte) {
	t.Helper()
	redemptions := 0
	transport := s.app.config.Transport
	s.app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/enroll") {
			redemptions++
		}
		return transport.RoundTrip(r)
	})
	if code := s.app.run(t.Context(), []string{"join", s.invitation}); code != 1 || !strings.Contains(s.diagnostic.String(), "fresh clone") || !strings.Contains(s.diagnostic.String(), "oberth repo add") {
		t.Fatal("cross-installation join did not explain the remedy", code, s.diagnostic.String())
	}
	if _, err := os.Stat(filepath.Join(s.app.base, "oberth.local")); !os.IsNotExist(err) {
		t.Fatal("refused join redeemed and published a profile", err)
	}
	after, err := os.ReadFile(filepath.Join(s.app.directory, ".git", "config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused join changed checkout", err)
	}
	after, err = client.ReadPrivate(s.invitation)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("refused join spent invitation", err)
	}
	if redemptions != 0 {
		t.Fatal("refused join redeemed invitation", redemptions)
	}
}
