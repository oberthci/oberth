package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

const requiredTestContract = "version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n"

type policyTestGit struct {
	*controlGit
	candidate, baseline       []byte
	candidateErr, baselineErr error
	reads                     []string
}

type policyChangingJobs struct {
	*fakeJobs
	afterBuild func()
}

func (jobs *policyChangingJobs) Wait(ctx context.Context, name string, output io.Writer) (JobResult, error) {
	result, err := jobs.fakeJobs.Wait(ctx, name, output)
	jobs.afterBuild()
	return result, err
}

func TestTrustedPolicyRechecksBeforePublication(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
	git := &policyTestGit{controlGit: fixture.git}
	fixture.scheduler.git = git
	fixture.scheduler.jobs = &policyChangingJobs{fakeJobs: fixture.jobs, afterBuild: func() {
		git.baseline = []byte(requiredTestContract)
	}}
	queued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "changed-policy", Repository: fixture.repo, Branch: "feature/test",
		SHA: strings.Repeat("a", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(context.Background(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunFailed || len(fixture.jobs.createdCI) != 1 || len(fixture.git.syncedBranches) != 0 {
		t.Fatalf("new upstream requirement bypassed by green build: status=%s builds=%d pushes=%v", run.Status, len(fixture.jobs.createdCI), fixture.git.syncedBranches)
	}
	if !strings.Contains(run.Error, "protected suite executor") {
		t.Fatalf("failure = %q", run.Error)
	}
}

func TestTrustedPolicyPublicationRecoveryAndFastForwardRefuseMissingEvidence(t *testing.T) {
	for _, promotion := range []bool{false, true} {
		t.Run(map[bool]string{false: "run outbox", true: "fast forward without run"}[promotion], func(t *testing.T) {
			ctx := context.Background()
			fixture := newControlFixture(t)
			git := &policyTestGit{controlGit: fixture.git, baseline: []byte(requiredTestContract)}
			fixture.scheduler.git = git
			var publication model.Publication
			var err error
			if promotion {
				_, publication = beginPendingPromotionPublication(t, fixture, "main", strings.Repeat("b", 40), strings.Repeat("a", 40))
			} else {
				_, err = fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch,
					Ref: "feature/recovery", SHA: strings.Repeat("a", 40), Actor: "agent@host", Trigger: "branch", TestedSHA: strings.Repeat("a", 40)})
				if err != nil {
					t.Fatal(err)
				}
				run, claimErr := fixture.store.ClaimNextRun(ctx)
				if claimErr != nil {
					t.Fatal(claimErr)
				}
				publication, err = fixture.scheduler.beginRunPublication(ctx, fixture.repo, run, model.Promotion{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.scheduler.recoverPendingPublications(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := fixture.store.Publication(ctx, publication.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != model.PublicationFailed || len(fixture.git.syncedBranches) != 0 || len(fixture.git.promotions) != 0 {
				t.Fatalf("recovered outbox bypassed policy: %#v pushes=%v promotions=%v", got, fixture.git.syncedBranches, fixture.git.promotions)
			}
		})
	}
}

func TestTrustedPolicyReadsExactTestedCommitAndQualifiedRepository(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed})
	git := &policyTestGit{controlGit: fixture.git}
	fixture.scheduler.git = git
	const testedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	queued, err := fixture.scheduler.AdmitRelease(context.Background(), ReleaseRequest{
		EventID: "tested-tag-policy", Repository: fixture.repo, Tag: "v1.0.0",
		ObjectSHA: strings.Repeat("a", 40), CommitSHA: testedSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(context.Background(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunPassed || len(fixture.jobs.createdReleases) != 1 || len(fixture.git.syncedTags) != 1 {
		t.Fatalf("policy-free release changed: status=%s builds=%d pushes=%v", run.Status, len(fixture.jobs.createdReleases), fixture.git.syncedTags)
	}
	want := "codeberg/acme/oberth:" + testedSHA + ":" + vmrunner.ContractFile
	if len(git.reads) != 4 || git.reads[0] != want || git.reads[2] != want {
		t.Fatalf("policy reads = %v, want immutable tested commit %s at both boundaries", git.reads, want)
	}
}

func (git *policyTestGit) ReadOptionalBlob(_ context.Context, repo, sha, file string, limit int) ([]byte, bool, error) {
	git.reads = append(git.reads, repo+":"+sha+":"+file)
	if file != vmrunner.ContractFile || limit != vmrunner.MaxContractBytes {
		return nil, false, errors.New("unexpected policy read")
	}
	return git.candidate, git.candidate != nil, git.candidateErr
}

func (git *policyTestGit) ReadDefaultBlob(_ context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	git.reads = append(git.reads, repo+":upstream:"+file)
	if file != vmrunner.ContractFile || limit != vmrunner.MaxContractBytes {
		return "", nil, false, errors.New("unexpected policy read")
	}
	return strings.Repeat("b", 40), git.baseline, git.baseline != nil, git.baselineErr
}

func TestTrustedPolicyRefusesBuildWithoutProtectedExecutor(t *testing.T) {
	for _, test := range []struct {
		name                      string
		candidate, baseline       []byte
		candidateErr, baselineErr error
	}{
		{name: "candidate requires suite", candidate: []byte(requiredTestContract)},
		{name: "candidate deleted published requirement", baseline: []byte(requiredTestContract)},
		{name: "candidate adds commands", candidate: []byte(requiredTestContract + "command: echo pass\n")},
		{name: "candidate object unreadable", candidateErr: errors.New("object unavailable")},
		{name: "upstream unavailable", baselineErr: errors.New("upstream unavailable")},
		{name: "invalid published requirement", baseline: []byte("version: 99\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t, JobResult{Status: model.RunPassed})
			git := &policyTestGit{controlGit: fixture.git, candidate: test.candidate, baseline: test.baseline,
				candidateErr: test.candidateErr, baselineErr: test.baselineErr}
			fixture.scheduler.git = git
			queued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
				EventID: "trusted-policy", Repository: fixture.repo, Branch: "feature/test",
				SHA: strings.Repeat("a", 40), Actor: "agent@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			run, err := fixture.store.Run(context.Background(), queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != model.RunFailed || len(fixture.jobs.createdCI) != 0 || len(fixture.git.syncedBranches) != 0 {
				t.Fatalf("required/unreadable policy ran or published: status=%s builds=%d pushes=%v", run.Status, len(fixture.jobs.createdCI), fixture.git.syncedBranches)
			}
			if !strings.Contains(run.Error, "trusted test") {
				t.Fatalf("missing actionable failure: %q", run.Error)
			}
		})
	}
}
