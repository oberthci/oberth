package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestRepoAddNewUpstreamRequiresAnOrganizationInThePath(t *testing.T) {
	for _, path := range []string{"widget.git", "/widget.git/"} {
		t.Run(path, func(t *testing.T) {
			b := fake.Bindings()
			command := urlCheckoutCommand(t, b, false)
			command.address = "ssh://forge.oberth.ci/" + path
			config := filepath.Join(command.app.directory, ".git", "config")
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if code := command.run(t); code != 1 || !strings.Contains(command.diagnostic.String(), "organization/repository") {
				t.Fatal(code, command.diagnostic.String())
			}
			after, err := os.ReadFile(config)
			if err != nil || string(before) != string(after) {
				t.Fatal("refusal changed checkout", err)
			}
			assertNoRepositoryMutation(t, b)
		})
	}
}

func assertNoRepositoryMutation(t *testing.T, b apicontract.Bindings) {
	t.Helper()
	_, _, adds := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
	_, _, repos := b.RepoAdd.Handler.(*fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]).Observed()
	_, _, verifies := b.RepoVerify.Handler.(*fake.Handler[apicontract.RepoVerifyArgs, apicontract.Repository]).Observed()
	if adds+repos+verifies != 0 {
		t.Fatal("refusal attempted registration or verification", adds, repos, verifies)
	}
}
