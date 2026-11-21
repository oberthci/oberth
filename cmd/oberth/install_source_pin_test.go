package main

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func installationChoicesFor(t *testing.T, name string, args ...string) (installationRequest, error) {
	t.Helper()
	for _, row := range installationCommands() {
		if row.name == name {
			flags := installationFlags(name)
			rest, err := installationArgs(row, flags, args)
			return installationRequest{name, flags, rest}, err
		}
	}
	t.Fatal("no installation command", name)
	return installationRequest{}, nil
}

// Install and upgrade read repeated pairs in order, and the explicit clear;
// a value that is not one pair is refused before the engine opens.
func TestSourcePinFlagsReachTheInstallerChoices(t *testing.T) {
	for _, name := range []string{"install", "upgrade"} {
		r, err := installationChoicesFor(t, name, "--source-pin", "origin/team/cli=origin/team/cli", "--source-pin=origin/team/cli=origin/team/lib", "--no-source-pins")
		if err != nil {
			t.Fatal(name, err)
		}
		c := r.choices()
		want := workflowpolicy.SourcePolicy{"origin/team/cli": {"origin/team/cli", "origin/team/lib"}}
		if !reflect.DeepEqual(c.SourcePins, want) || !c.ClearSourcePins {
			t.Fatal(name, "choices lost the pairs", c.SourcePins, c.ClearSourcePins)
		}
		if c := mustChoices(t, name); c.SourcePins != nil || c.ClearSourcePins {
			t.Fatal(name, "pairs set without the option", c.SourcePins)
		}
		for _, bad := range []string{"origin/team/cli", "=origin/team/cli", "origin/team/cli="} {
			if _, err := installationChoicesFor(t, name, "--source-pin", bad); err == nil {
				t.Fatal(name, "accepted", bad)
			}
		}
	}
	if _, err := installationChoicesFor(t, "rollback", "--source-pin", "origin/team/cli=origin/team/cli"); err == nil {
		t.Fatal("rollback accepted a source pin")
	}
}

func mustChoices(t *testing.T, name string) installer.Choices {
	t.Helper()
	r, err := installationChoicesFor(t, name)
	if err != nil {
		t.Fatal(err)
	}
	return r.choices()
}
