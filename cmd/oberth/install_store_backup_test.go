package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestDatabaseOnlyBackupGuidanceNamesCoverageAndRestoreLimit(t *testing.T) {
	const hint = "Database-only backups exclude store data; keep secure originals to put again."
	if installer.StoreSnapshotHint != hint || len(hint) > 80 {
		t.Fatal(installer.StoreSnapshotHint)
	}
	for _, path := range []string{"../../docs/install.md", "../../docs/operations.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, required := range []string{"store data", "installation Secrets", "restore", "put", "again", "recovery", "unseal", "cannot restore", "oberth access list --repo <repo> --kind store_path --json"} {
			if !strings.Contains(text, required) {
				t.Fatal(path, "missing recovery fact", required)
			}
		}
	}
}

func TestDocumentedSecretNameListUsesRepositoryGrants(t *testing.T) {
	section := installDocSection(t, "## Back up")
	_, command, found := strings.Cut(section, "`oberth access list ")
	command, _, closed := strings.Cut(command, "`")
	if !found || !closed {
		t.Fatal("missing documented list command")
	}
	words := strings.Fields(strings.ReplaceAll("oberth access list "+command, "<repo>", "origin/team/app"))
	bindings := fake.Bindings()
	handler := &fake.Handler[apicontract.AccessListArgs, apicontract.GrantPage]{Value: apicontract.GrantPage{Grants: []apicontract.Grant{{Repo: "origin/team/app", File: ".oberth/build.yaml", Step: "publish", Path: "registry/password", Status: "approved"}}}}
	bindings.AccessList.Handler = handler
	app, out, diagnostic := fixture(t, bindings)
	if code := app.run(t.Context(), words[1:]); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	inputs, _, calls := handler.Observed()
	if calls != 1 || len(inputs) != 1 || inputs[0].Repo != "origin/team/app" || inputs[0].Kind != string(stateport.GrantStorePath) {
		t.Fatal("wrong secret-name query", inputs, calls)
	}
	var page apicontract.GrantPage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err, out.String())
	}
	if len(page.Grants) != 1 || page.Grants[0].Path != "registry/password" || page.Grants[0].Step != "publish" {
		t.Fatal("secret recovery tuple lost", out.String())
	}
}
