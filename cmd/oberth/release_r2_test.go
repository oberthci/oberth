package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type releaseFixture struct {
	app             application
	out, diagnostic *bytes.Buffer
	sha             string
	check           *fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]
	wait            *fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]
	events          *fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]
	tags, pushes    int
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	directory := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"commit", "--allow-empty", "-m", "head"}} {
		if _, err := runGit(t.Context(), directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := runGit(t.Context(), directory, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runGit(t.Context(), directory, "update-ref", "refs/remotes/oberth/main", sha); err != nil {
		t.Fatal(err)
	}
	if _, err = runGit(t.Context(), directory, "config", "oberth.repository", "project"); err != nil {
		t.Fatal(err)
	}
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: "main"}}}}
	checked := &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v0.1.0", CommitSHA: &sha}}
	waited := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release", Status: stepmodel.RunPassed, CommitSHA: &sha}}}
	b.RunCheck.Handler, b.Wait.Handler = checked, waited
	events := &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: apicontract.EventResult{EventPage: model.EventPage{Events: []model.AgentEvent{{ID: 1, Kind: string(model.EventReleasePublished), RunID: "release"}}}}}
	b.EventsWait.Handler = events
	app, out, diagnostic := fixture(t, b)
	app.directory = directory
	f := &releaseFixture{app: app, out: out, diagnostic: diagnostic, sha: sha, check: checked, wait: waited, events: events}
	f.app.git = func(ctx context.Context, args []string) (string, error) {
		switch args[0] {
		case "fetch":
			return "", nil
		case "push":
			f.pushes++
			return "", nil
		case "tag":
			f.tags++
		}
		return runGit(ctx, directory, args...)
	}
	return f
}
func (f *releaseFixture) run(t *testing.T) int {
	t.Helper()
	return f.app.run(t.Context(), []string{"release", "v0.1.0"})
}
func assertNoReleaseEffects(t *testing.T, f *releaseFixture) {
	t.Helper()
	if f.tags != 0 || f.pushes != 0 {
		t.Fatal("refused preflight changed Git refs")
	}
	if _, err := runGit(t.Context(), f.app.directory, "rev-parse", "--verify", "--quiet", "refs/tags/v0.1.0"); err == nil {
		t.Fatal("refused release left a local tag")
	}
}
func TestReleaseGrantRefusalThenSameTagRetry(t *testing.T) {
	f := newReleaseFixture(t)
	command := "oberth access allow 'forge/org/repo' '.oberth/release.yaml' 'publish' '" + strings.Repeat("long-path/", 18) + "key'"
	f.check.Value = apicontract.RunCheckResult{OK: false, ReleaseTag: "v0.1.0", CommitSHA: &f.sha, Findings: []apicontract.Finding{{Code: "grant", Reason: "Secret access requires approval.", Remedy: "Run the exact approval command.", Command: command}}}
	if code := f.run(t); code != 1 {
		t.Fatal("release ignored missing grant")
	}
	assertNoReleaseEffects(t, f)
	assertExactReleaseRemedy(t, f.diagnostic.String(), command)
	f.check.Value.OK = true
	f.check.Value.Findings = nil
	if code := f.run(t); code != 0 {
		t.Fatalf("approved same-tag retry failed: %s", f.diagnostic.String())
	}
	first, err := runGit(t.Context(), f.app.directory, "rev-parse", "refs/tags/v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if code := f.run(t); code != 0 {
		t.Fatalf("existing same-commit tag refused: %s", f.diagnostic.String())
	}
	second, err := runGit(t.Context(), f.app.directory, "rev-parse", "refs/tags/v0.1.0")
	if err != nil || first != second || f.tags != 1 || f.pushes != 2 {
		t.Fatal("retry recreated/moved tag or did not continue")
	}
	inputs, _, calls := f.check.Observed()
	if calls != 3 || inputs[0].ReleaseTag != "v0.1.0" || inputs[0].Ref != f.sha {
		t.Fatal("release-specific exact-source preflight missing")
	}
}
func TestReleaseRefusesWrongHeadOrTestEvidence(t *testing.T) {
	for _, kind := range []string{"head", "failed", "untested", "wrong-run", "ordinary-check", "wrong-tag-check", "wrong-check-commit"} {
		t.Run(kind, func(t *testing.T) { refuseReleaseEvidence(t, kind) })
	}
}
func refuseReleaseEvidence(t *testing.T, kind string) {
	t.Helper()
	f := newReleaseFixture(t)
	other := strings.Repeat("b", 40)
	switch kind {
	case "head":
		if _, err := runGit(t.Context(), f.app.directory, "commit", "--allow-empty", "-m", "other"); err != nil {
			t.Fatal(err)
		}
	case "failed":
		f.wait.Value.Run.Status = stepmodel.RunFailed
	case "untested":
		f.wait.Value.Run.CommitSHA = nil
	case "wrong-run":
		f.wait.Value.Run.CommitSHA = &other
	case "ordinary-check":
		f.check.Value.ReleaseTag = ""
	case "wrong-tag-check":
		f.check.Value.ReleaseTag = "v9.9.9"
	case "wrong-check-commit":
		f.check.Value.CommitSHA = &other
	}
	if code := f.run(t); code != 1 {
		t.Fatalf("%s was accepted", kind)
	}
	assertNoReleaseEffects(t, f)
}
func TestReleaseNeverMovesTagOnAnotherCommit(t *testing.T) {
	f := newReleaseFixture(t)
	if _, err := runGit(t.Context(), f.app.directory, "commit", "--allow-empty", "-m", "other"); err != nil {
		t.Fatal(err)
	}
	other, err := runGit(t.Context(), f.app.directory, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runGit(t.Context(), f.app.directory, "tag", "-a", "-m", "existing", "v0.1.0", other); err != nil {
		t.Fatal(err)
	}
	if _, err = runGit(t.Context(), f.app.directory, "checkout", "--detach", f.sha); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t); code != 1 || !strings.Contains(f.diagnostic.String(), "tag points to another commit") {
		t.Fatal("conflicting release tag accepted")
	}
	current, err := runGit(t.Context(), f.app.directory, "rev-parse", "refs/tags/v0.1.0^{commit}")
	if err != nil || current != other || f.tags != 0 || f.pushes != 0 {
		t.Fatal("conflicting tag was moved or pushed")
	}
}

func assertExactReleaseRemedy(t *testing.T, diagnostic, command string) {
	t.Helper()
	for _, line := range strings.Split(diagnostic, "\n") {
		if line == command {
			return
		}
	}
	t.Fatalf("exact remedy command lost or wrapped: %s", diagnostic)
}

func TestReleaseTagReadFailureDoesNotCreateTag(t *testing.T) {
	f := newReleaseFixture(t)
	git := f.app.git
	f.app.git = func(ctx context.Context, args []string) (string, error) {
		if args[0] == "rev-parse" && strings.Contains(strings.Join(args, " "), "refs/tags/") {
			return "", errors.New("cannot read local tag")
		}
		return git(ctx, args)
	}
	if code := f.run(t); code != 1 {
		t.Fatal("tag read failure ignored")
	}
	assertNoReleaseEffects(t, f)
}

func TestReleaseTagReadExitTwoDoesNotCreateTag(t *testing.T) {
	f := newReleaseFixture(t)
	_, failure := exec.CommandContext(t.Context(), "sh", "-c", "exit 2").Output()
	if failure == nil {
		t.Fatal("exit fixture succeeded")
	}
	git := f.app.git
	f.app.git = func(ctx context.Context, args []string) (string, error) {
		if args[0] == "rev-parse" && strings.Contains(strings.Join(args, " "), "refs/tags/") {
			return "", flowError("git", failure)
		}
		return git(ctx, args)
	}
	if f.run(t) != 1 {
		t.Fatal("non-missing tag failure ignored")
	}
	assertNoReleaseEffects(t, f)
}
