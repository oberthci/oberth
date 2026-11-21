package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnappliedInstallationLeavesNoProfileDirectory(t *testing.T) {
	for _, operation := range []string{"decline", "refuse", "dry-run"} {
		base := filepath.Join(t.TempDir(), "new-config")
		t.Run(operation, func(t *testing.T) {
			engine, charts := offlineInstallEngine(t, base)
			flags := installationFlags("install")
			args := []string{"--context=chosen", "--chart=../../charts/oberth"}
			if operation == "dry-run" {
				args = append(args, "--dry-run")
			}
			if operation == "refuse" {
				engine.Cluster.Config.Host = "https://0.0.0.0:6443"
			}
			if err := flags.Parse(args); err != nil {
				t.Fatal(err)
			}
			r := installationRequest{name: "install", flags: flags}
			s := installationSession{engine: engine, request: r, choices: r.choices()}
			var out bytes.Buffer
			err := s.applyInstallation(t.Context(), strings.NewReader("n\n"), &out)
			if (err != nil) != (operation == "refuse") {
				t.Fatal(err)
			}
			if len(charts.Applies) != 0 {
				t.Fatal("unapproved apply")
			}
		})
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatal(operation, "left a profile or parent directory", err)
		}
	}
}
