package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestUplinkMissingScopeRefusesBeforeSavingCredentials(t *testing.T) {
	for _, extra := range [][]string{nil, {"--scope", "[]"}, {"--scope", "null"}, {"--json"}} {
		b := fake.Bindings()
		h := &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{}
		b.UplinkAdd.Handler = h
		app, _, diagnostic := fixture(t, b)
		path := filepath.Join(decidingDirectory(t), "private-output")
		args := append([]string{"uplink", "add", "--identity", "browser", "--bearer-file", path}, extra...)
		if code := app.run(t.Context(), args); code == 0 || !strings.Contains(diagnostic.String(), "--scope") {
			t.Fatal("missing scope accepted or remedy absent", code, diagnostic.String())
		}
		_, _, calls := h.Observed()
		if calls != 0 {
			t.Fatal("refused scope reached registration")
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("refused scope wrote private output", err)
		}
	}
}
