package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// failedRelease runs oberth release v0.1.0 against a server whose release run
// ends as given, or does not exist when status is "".
func failedRelease(t *testing.T, status stepmodel.RunStatus) string {
	t.Helper()
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: "main"}}}}
	wait := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{}
	wait.Run = func(context.Context) (apicontract.WaitResult, error) {
		inputs, _, _ := wait.Observed()
		if inputs[len(inputs)-1].Ref == "" {
			return apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "main-run", Status: stepmodel.RunPassed, CommitSHA: ptrReleaseSHA()}}, nil
		}
		if status == "" {
			return apicontract.WaitResult{}, publicerr.New(publicerr.NotFound, "Run not found.", "List runs with oberth run list and select a visible run.", nil)
		}
		return apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release-run", Status: status, CommitSHA: ptrReleaseSHA()}}, nil
	}
	b.Wait.Handler = wait
	b.RunCheck.Handler = &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v0.1.0", CommitSHA: ptrReleaseSHA()}}
	app, _, diagnostic := fixture(t, b)
	app.git = func(_ context.Context, args []string) (string, error) {
		if args[0] == "config" {
			return "project", nil
		}
		return strings.Repeat("a", 40), nil
	}
	if code := app.run(context.Background(), []string{"release", "v0.1.0"}); code != 1 {
		t.Fatal("an unfinished release succeeded", code, diagnostic.String())
	}
	return diagnostic.String()
}

func TestReleaseOfATagWithoutARunSaysTheVersionIsUsed(t *testing.T) {
	text := failedRelease(t, "")
	for _, want := range []string{"The release tag already exists, but it has no release run.", "Its version is used; release the next version.", "Used version:\nv0.1.0"} {
		if !strings.Contains(text, want) {
			t.Fatalf("release without a run does not say %q: %s", want, text)
		}
	}
}
