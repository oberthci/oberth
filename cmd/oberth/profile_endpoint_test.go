package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallerProfileUsesTheClientSSHEndpoint(t *testing.T) {
	installed := installer.Profile{ServerURL: "https://oberth.local", SSHURL: "ssh://git@oberth.local:2222"}
	raw, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	var profile client.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.SSHURL != installed.SSHURL || strings.Contains(string(raw), "git_url") {
		t.Fatal("installer/client endpoint contract differs", string(raw))
	}
	if _, err := client.ValidateSSHEndpoint(profile.SSHURL, profile.ServerURL); err != nil {
		t.Fatal(err)
	}
}
