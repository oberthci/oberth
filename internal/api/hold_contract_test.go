package api_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestBInspectionSurfaceContract(t *testing.T) {
	f := setup(t, nil)
	registry, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	tools := listTools(t, registry, admin())
	for _, name := range []string{"run_continue", "run_hold_patch"} {
		if _, ok := apicontract.Required(name); ok {
			t.Fatal("removed tool remains", name)
		}
		for _, tool := range tools {
			if tool.Name == name {
				t.Fatal("removed tool advertised", name)
			}
		}
		if w := rpc(f, name, map[string]any{"id": "12345678"}); !strings.Contains(w.Body.String(), "error") && !strings.Contains(w.Body.String(), "isError") {
			t.Fatal("removed tool remains callable", name)
		}
	}
	for _, tool := range tools {
		if tool.Name != "run_hold_exec" {
			continue
		}
		for _, word := range []string{"any command", "identity and mounts", "It can change files, including outputs Argo collects for downstream steps.", "secret-free", "can no longer count as green", "remains if dispatch fails", "argument count and SHA-256", "never the command arguments"} {
			if !strings.Contains(tool.Description, word) {
				t.Fatal("inspection warning missing", word)
			}
		}
	}
}
