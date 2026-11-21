package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestSharedForgeKeyDoesNotShareEndpointConfiguration(t *testing.T) {
	o := &serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}
	o.c.config.Files.ForgeKey, _ = composeSSHKey(t, t.TempDir(), "mounted-forge-key")
	a := serverAdministration{owner: o}
	first, err := a.configure(t.Context(), stateport.Upstream{Name: "first", BaseURL: "ssh://first.oberth.ci", KeyName: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.configure(t.Context(), stateport.Upstream{Name: "second", BaseURL: "ssh://second.oberth.ci:2200", KeyName: "shared"})
	if err != nil || first == second {
		t.Fatal("shared key overwrote another endpoint's configuration", err)
	}
	for path, host := range map[string]string{first: "first.oberth.ci", second: "second.oberth.ci"} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "HostName "+host+"\n") {
			t.Fatal("another upstream changed the pinned endpoint", err)
		}
	}
}
