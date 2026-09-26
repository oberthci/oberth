package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func annotatedWaitTag(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{
			"-C", dir, "-c", "user.name=Wait Test", "-c", "user.email=wait@example.test",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	git("commit", "--allow-empty", "--quiet", "-m", "release fixture")
	git("tag", "-a", "v1.0.0", "-m", "annotated release")
	objectSHA := git("rev-parse", "refs/tags/v1.0.0")
	commitSHA := git("rev-parse", "refs/tags/v1.0.0^{commit}")
	if objectSHA == commitSHA {
		t.Fatal("annotated tag fixture must have distinct object and commit identities")
	}
	return objectSHA, commitSHA
}

func finishWaitFixtureRun(t *testing.T, fixture *controlFixture, runID string) {
	t.Helper()
	claimed, err := fixture.store.ClaimNextRun(context.Background())
	if err != nil || claimed.ID != runID {
		t.Fatalf("claim = %s, %v; want %s", claimed.ID, err, runID)
	}
	if _, err := fixture.store.FinishRun(context.Background(), runID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
}

func admitWaitFixtureRelease(t *testing.T, fixture *controlFixture, objectSHA, commitSHA string) model.Run {
	t.Helper()
	result, err := fixture.scheduler.AdmitRelease(context.Background(), ReleaseRequest{
		EventID: "annotated-release", Repository: fixture.repo, Tag: "v1.0.0",
		ObjectSHA: objectSHA, CommitSHA: commitSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Run
}

func callWaitFixture(t *testing.T, control *API, repo, sha, trigger string) (WaitResponse, error) {
	t.Helper()
	arguments, err := json.Marshal(map[string]string{"repo": repo, "sha": sha, "trigger": trigger})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "wait", arguments)
	if err != nil {
		return WaitResponse{}, err
	}
	response, ok := value.(WaitResponse)
	if !ok {
		t.Fatalf("wait result has type %T", value)
	}
	return response, nil
}

func TestWaitResolvesAnnotatedTagAndPeeledCommit(t *testing.T) {
	fixture := newControlFixture(t)
	objectSHA, commitSHA := annotatedWaitTag(t)
	branch, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "initial-branch", Repository: fixture.repo, Branch: "feature/release", SHA: commitSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishWaitFixtureRun(t, fixture, branch.ID)
	release := admitWaitFixtureRelease(t, fixture, objectSHA, commitSHA)
	control := fixture.api(t)
	for _, sha := range []string{objectSHA, commitSHA} {
		response, err := callWaitFixture(t, control, fixture.repo.Name, sha, "release")
		if err != nil || response.Run.ID != release.ID || !response.StillRunning || response.Run.Status != model.RunQueued {
			t.Fatalf("pending release via %s = %#v, %v", sha, response, err)
		}
	}
	finishWaitFixtureRun(t, fixture, release.ID)
	// Newer opposite-trigger work for the same commit, and a newer unrelated
	// tag, must not hide the completed release selected by either identity.
	newerBranch, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "newer-branch", Repository: fixture.repo, Branch: "feature/next", SHA: commitSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v2.0.0", Trigger: "tag", Release: true,
		SHA: strings.Repeat("e", 40), TestedSHA: strings.Repeat("f", 40), Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "promotion/next", Trigger: "promotion",
		SHA: commitSHA, TestedSHA: commitSHA, Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"release", "tag"} {
		for _, sha := range []string{objectSHA, commitSHA, objectSHA[:12], commitSHA[:12], strings.ToUpper(commitSHA)} {
			t.Run(trigger+"/"+sha, func(t *testing.T) {
				response, err := callWaitFixture(t, control, fixture.repo.Name, sha, trigger)
				if err != nil || response.StillRunning || response.Run.ID != release.ID || response.Run.Status != model.RunPassed {
					t.Fatalf("completed release = %#v, %v", response, err)
				}
				if response.SHA != objectSHA || response.Run.TestedSHA != commitSHA || response.Ref != "v1.0.0" {
					t.Fatalf("release identities changed: %#v", response.Run)
				}
			})
		}
	}
	response, err := callWaitFixture(t, control, fixture.repo.Name, commitSHA, "ci")
	if err != nil || response.Run.ID != newerBranch.ID || !response.StillRunning {
		t.Fatalf("branch alias selected the release: %#v, %v", response, err)
	}
	if _, err := callWaitFixture(t, control, fixture.repo.Name, strings.Repeat("9", 40), "release"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown SHA error = %v, want not found", err)
	}

	other, err := fixture.store.CreateRepository(context.Background(), model.RepositorySpec{
		Name: "other", UpstreamID: fixture.repo.UpstreamID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: other.ID, RefKind: model.RefTag, Ref: "v1.0.0", Trigger: "tag", Release: true,
		SHA: objectSHA, TestedSHA: commitSHA, Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callWaitFixture(t, control, "", commitSHA, "release"); !errors.Is(err, ErrAmbiguousRepository) {
		t.Fatalf("cross-repository SHA error = %v, want ambiguity", err)
	}
	response, err = callWaitFixture(t, control, fixture.repo.Name, commitSHA, "release")
	if err != nil || response.Run.ID != release.ID || response.StillRunning {
		t.Fatalf("repository-qualified release = %#v, %v", response, err)
	}
}

type observedWaitStore struct {
	*store.Store
	resolved chan struct{}
}

func (database observedWaitStore) ResolveWaitRun(ctx context.Context, repoID int64, selector, trigger string) (model.Run, error) {
	run, err := database.Store.ResolveWaitRun(ctx, repoID, selector, trigger)
	select {
	case database.resolved <- struct{}{}:
	default:
	}
	return run, err
}

func TestWaitWakesForReleaseAdmittedAfterTerminalBranch(t *testing.T) {
	fixture := newControlFixture(t)
	objectSHA, commitSHA := annotatedWaitTag(t)
	branch, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "initial-branch", Repository: fixture.repo, Branch: "feature/release", SHA: commitSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishWaitFixtureRun(t, fixture, branch.ID)
	observed := observedWaitStore{Store: fixture.store, resolved: make(chan struct{}, 1)}
	control, err := NewAPI(APIConfig{Runs: observed, Signals: fixture.signals, MaximumWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		value any
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		value, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "wait",
			json.RawMessage(fmt.Sprintf(`{"sha":%q,"trigger":"release"}`, commitSHA)))
		finished <- result{value: value, err: err}
	}()
	select {
	case <-observed.resolved:
	case <-ctx.Done():
		t.Fatal("wait never looked up the initial branch")
	}
	release := admitWaitFixtureRelease(t, fixture, objectSHA, commitSHA)
	finishWaitFixtureRun(t, fixture, release.ID)
	fixture.signals.NotifyRun(release.ID)
	select {
	case result := <-finished:
		response, ok := result.value.(WaitResponse)
		if result.err != nil || !ok || response.StillRunning || response.Run.ID != release.ID {
			t.Fatalf("wait did not observe newly completed release: %#v, %v", result.value, result.err)
		}
	case <-ctx.Done():
		t.Fatal("wait did not wake after release completion")
	}
}
