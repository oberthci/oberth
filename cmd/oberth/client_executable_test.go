package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoAddRefusesUnselectedVersionInCheckoutWords(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "superseded", true: "missing-link"}[missing], func(t *testing.T) {
			checkUnselectedClientVersion(t, missing)
		})
	}
}

func checkUnselectedClientVersion(t *testing.T, missing bool) {
	t.Helper()
	command, _ := bindingRepoCommand(t, false)
	stable, old, newer := upgradeBinaries(t)
	if err := os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if !missing {
		if err := os.Symlink(newer, stable); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	command.app.executable = func() (string, error) { return old, nil }
	if code := command.run(t); code != 1 {
		t.Fatalf("unselected version exit=%d", code)
	}
	message := command.diagnostic.String()
	if !strings.Contains(message, "Cannot bind this checkout") || !strings.Contains(message, "run oberth repo add") || strings.Contains(message, "oberth mcp") {
		t.Fatal("refusal does not name checkout recovery", message)
	}
	assertBindingUnwritten(t, command, before)
	if missing {
		if err = os.Symlink(newer, stable); err != nil {
			t.Fatal(err)
		}
	}
	command.app.executable = func() (string, error) { return stable, nil }
	if code := command.run(t); code != 0 {
		t.Fatal("installed command remedy failed", code, command.diagnostic.String())
	}
}
