package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

// A flag a command does not take is named, for installation and flow commands.
func TestUnknownFlagIsNamed(t *testing.T) {
	for _, args := range [][]string{
		{"upgrade", "--dry-run", "--context", "k3d-next-dev", "--profile", "k3d-next-dev"},
		{"rollback", "--dry-run", "--context=k3d-next-dev"},
		{"promote", "--context", "k3d-next-dev"},
	} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		if code := app.run(context.Background(), args); code != 2 || !strings.Contains(diagnostic.String(), "Unknown flag --context; use help for this command.") {
			t.Fatal("unknown flag not named", args, code, diagnostic.String())
		}
	}
}
