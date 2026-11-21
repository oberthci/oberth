package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestServerStateRefusalExplainsPermissionsByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.key")
	if err := os.WriteFile(path, []byte("private fixture"), 0664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	var problem *publicerr.Error
	if err := checkServerState(path, false); !errors.As(err, &problem) {
		t.Fatal("unsafe state accepted", err)
	}
	if publicerr.CheckMessage(problem.Reason(), 40) != nil || publicerr.CheckMessage(problem.Remedy(), 120) != nil {
		t.Fatal("permission refusal exceeds display bounds")
	}
	app, _, diagnostic := fixture(t, fake.Bindings())
	raw, err := json.Marshal(map[string]any{"reason": problem.Reason(), "remedy": problem.Remedy(), "context": problem.Context})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.renderRefusal(raw, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	want := problem.Reason() + "\n" + "Restore the server's ownership and group; remove other access and group execute\n  access to files." + "\nFile:\n" + path + "\n"
	if diagnostic.String() != want {
		t.Fatal("default renderer hid the path or remedy", diagnostic.String())
	}
	if !strings.Contains(diagnostic.String(), "Restore the server's ownership") || !strings.Contains(diagnostic.String(), "remove other access") {
		t.Fatal("refusal does not explain repair", diagnostic.String())
	}
}
