package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestDecidingRepositoryAddressRefusesBeforeCheckout(t *testing.T) {
	for _, address := range []string{"ssh:///team/widget.git", "ssh://forge.oberth.ci"} {
		t.Run(address, func(t *testing.T) {
			app, _, diagnostic := fixture(t, fake.Bindings())
			gitCalls := 0
			app.git = func(context.Context, []string) (string, error) { gitCalls++; return "checkout", nil }
			if code := app.run(t.Context(), []string{"repo", "add", address}); code != 1 || gitCalls != 0 {
				t.Fatalf("invalid address reached checkout: exit=%d calls=%d diagnostic=%s", code, gitCalls, diagnostic.String())
			}
		})
	}
}

func TestDecidingRepositoryRegistrationStopsBeforeBinding(t *testing.T) {
	for _, failure := range []string{"empty-reply", "verification-refused"} {
		t.Run(failure, func(t *testing.T) {
			b := fake.Bindings()
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{{Name: "source", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}}}
			added := &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "source/team/widget"}}
			verified := &fake.Handler[apicontract.RepoVerifyArgs, apicontract.Repository]{}
			b.RepoAdd.Handler, b.RepoVerify.Handler = added, verified
			want := "A required service protocol is unavailable.\nUse a server and CLI build with the required operation contract.\n"
			if failure == "empty-reply" {
				added.Value.Repo = ""
			} else {
				verified.PrepareError = publicerr.New(publicerr.Unavailable, "Repository verification refused.", "Correct forge access and retry.", nil)
				want = "Repository verification refused.\nCorrect forge access and retry.\n"
			}
			command := urlCheckoutCommand(t, b, false)
			config := filepath.Join(command.app.directory, ".git", "config")
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if code := command.run(t, "--upstream", "source"); code != 1 || command.diagnostic.String() != want {
				t.Fatalf("registration failure: exit=%d diagnostic=%q want=%q", code, command.diagnostic.String(), want)
			}
			after, err := os.ReadFile(config)
			if err != nil || string(after) != string(before) {
				t.Fatal("refusal changed checkout", err)
			}
			_, _, calls := verified.Observed()
			if failure == "empty-reply" && calls != 0 {
				t.Fatal("empty reply reached verification")
			}
		})
	}
}
