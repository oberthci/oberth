package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points XDG_CONFIG_HOME at an empty directory, so no test reads the
// client files (~/.config/oberth) of the machine it runs on: every remote
// command now falls back to them (client.ResolveConnection).
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "oberth-cli-config-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
