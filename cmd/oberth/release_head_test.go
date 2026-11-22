package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// detachedAtMain is a checkout detached at main, whose oberth remote holds main.
func detachedAtMain(t *testing.T) (string, string) {
	t.Helper()
	dir, bare := t.TempDir(), filepath.Join(t.TempDir(), "server.git")
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Oberth"}, {"config", "user.email", "build@oberth.ci"}, {"commit", "--allow-empty", "-m", "first"}, {"clone", "--bare", dir, bare}, {"remote", "add", "oberth", bare}, {"config", "oberth.repository", "project"}, {"switch", "--detach", "main"}} {
		if _, err := runGit(context.Background(), dir, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	sha, err := runGit(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, sha
}

func passingRelease(branch, sha string) apicontract.Bindings {
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: branch}}}}
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release-run", Status: stepmodel.RunPassed, CommitSHA: &sha}}}
	b.RunCheck.Handler = &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v1.0.0", CommitSHA: &sha}}
	b.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: apicontract.EventResult{EventPage: model.EventPage{Events: []model.AgentEvent{{ID: 7, Kind: string(model.EventReleasePublished), RunID: "release-run"}}}}}
	return b
}

// The server may report the default branch as "main" or "refs/heads/main";
// a detached checkout at that head releases either way.
func TestReleaseFromADetachedCheckoutReadsEitherDefaultBranchForm(t *testing.T) {
	for _, branch := range []string{"main", "refs/heads/main"} {
		t.Run(branch, func(t *testing.T) {
			dir, sha := detachedAtMain(t)
			app, _, diagnostic := fixture(t, passingRelease(branch, sha))
			app.directory = dir
			app.git = func(ctx context.Context, args []string) (string, error) {
				if args[0] == "push" {
					return "", nil
				}
				return runGit(ctx, dir, args...)
			}
			code := app.run(context.Background(), []string{"release", "v1.0.0"})
			if code != 0 || strings.Contains(diagnostic.String(), "refs/heads/refs/heads/") {
				t.Fatalf("release from a detached default head failed: %d %s", code, diagnostic.String())
			}
		})
	}
}
