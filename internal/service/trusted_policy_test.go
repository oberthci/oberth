package service

import (
	"context"
	"encoding/json"
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
	if !strings.Contains(run.Error, "removed the upstream trusted test requirement") {
		t.Fatalf("failure = %q", run.Error)
	}
}

func TestTrustedPolicyPublicationRecoveryAndFastForwardRefuseMissingEvidence(t *testing.T) {
	for _, promotion := range []bool{false, true} {
		t.Run(map[bool]string{false: "run outbox", true: "fast forward without run"}[promotion], func(t *testing.T) {
			ctx := context.Background()
			fixture := newControlFixture(t)
			git := &policyTestGit{controlGit: fixture.git, candidate: []byte(requiredTestContract), baseline: []byte(requiredTestContract)}
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

// V2 decoding is a source foundation, not authority to execute or publish.
func TestTrustedKVMV2PolicyRemainsInactive(t *testing.T) {
	contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [ebpf/secret_monitor.o]\n")
	for _, baselineOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate", true: "removed baseline"}[baselineOnly], func(t *testing.T) {
			ctx := context.Background()
			fixture := newControlFixture(t, JobResult{Status: model.RunPassed})
			git := &policyTestGit{controlGit: fixture.git}
			if baselineOnly {
				git.baseline = contract
			} else {
				git.candidate = contract
			}
			fixture.scheduler.git = git
			wantError := "protected suite executor"
			if baselineOnly {
				wantError = "removed the upstream trusted test requirement"
			}
			sha := strings.Repeat("c", 40)
			queued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "v2-inactive", Repository: fixture.repo, Branch: "feature/v2", SHA: sha, Actor: "agent@host"})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.scheduler.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := fixture.store.Run(ctx, queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != model.RunFailed || len(fixture.jobs.createdCI) != 0 || len(fixture.git.syncedBranches) != 0 || !strings.Contains(run.Error, wantError) {
				t.Fatalf("v2 activated without runtime: %#v", run)
			}
			for _, publication := range []model.Publication{{ResultSHA: sha}, {ResultSHA: sha, RunID: run.ID}} {
				err := fixture.scheduler.requirePublicationTestPolicy(ctx, fixture.repo, publication)
				if err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("publication requires explicit unavailable executor: %v", err)
				}
			}
		})
	}
}

// Models operator recipe removal without changing the journaled passing receipt.
type rotatingPreflight struct {
	*protectedFixture
	suite, guest string
}

func (p *rotatingPreflight) Supports(c vmrunner.Contract) error {
	if c.SuiteSHA256 != p.suite || c.GuestSHA256 != p.guest {
		return errors.New("pair is not operator-approved")
	}
	return nil
}

func TestTrustedPinRotationRechecksPromotionAndRecovery(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
	baseline := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	candidate := []byte(strings.ReplaceAll(strings.ReplaceAll(string(baseline), strings.Repeat("a", 64), strings.Repeat("c", 64)), strings.Repeat("b", 64), strings.Repeat("d", 64)))
	fixture.scheduler.git = &policyTestGit{controlGit: fixture.git, candidate: candidate, baseline: baseline}
	executor := &rotatingPreflight{protectedFixture: &protectedFixture{store: fixture.store}, suite: strings.Repeat("c", 64), guest: strings.Repeat("d", 64)}
	fixture.scheduler.trusted = executor
	sha := strings.Repeat("e", 40)
	queued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "pin-rotation", Repository: fixture.repo, Branch: "feature/rotation", SHA: sha, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(ctx, queued.ID)
	if err != nil || run.Status != model.RunPassed || len(fixture.git.syncedBranches) != 1 {
		t.Fatalf("approved rotation did not pass CI: %#v, %v", run, err)
	}
	_, publication := beginPendingPromotionPublication(t, fixture, "main", strings.Repeat("b", 40), sha)
	before, _, err := fixture.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "trusted.pin_rotation.authorized", RepoID: fixture.repo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.requirePublicationTestPolicy(ctx, fixture.repo, publication); err != nil {
		t.Fatalf("approved green rotation cannot promote: %v", err)
	}
	actions, _, err := fixture.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "trusted.pin_rotation.authorized", RepoID: fixture.repo.ID})
	if err != nil || len(actions) != len(before)+1 {
		t.Fatalf("publication must append its own rotation audit: before=%d after=%d, %v", len(before), len(actions), err)
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(actions[0].Details), &details); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"candidate_sha": sha, "baseline_sha": strings.Repeat("b", 40), "from_suite_sha256": strings.Repeat("a", 64), "from_guest_sha256": strings.Repeat("b", 64), "to_suite_sha256": executor.suite, "to_guest_sha256": executor.guest} {
		if details[key] != want {
			t.Fatalf("audit %s = %q, want %q", key, details[key], want)
		}
	}
	for name, check := range map[string]func() error{
		"admission": func() error {
			_, err := fixture.scheduler.trustedRequirement(ctx, fixture.repo, sha)
			return err
		},
		"publication": func() error {
			return fixture.scheduler.requirePublicationTestPolicy(ctx, fixture.repo, publication)
		},
	} {
		t.Run(name+" audit unavailable", func(t *testing.T) {
			var events []string
			fixture.scheduler.auditor = &orderedAuditor{events: &events, rejectNext: true}
			if err := check(); err == nil || !strings.Contains(err.Error(), "audit trusted test pin rotation") {
				t.Fatalf("unaudited rotation admitted: %v", err)
			}
		})
	}
	fixture.scheduler.auditor = fixture.store
	// The same candidate and passing receipt lose authority when the new
	// pair is removed from the operator's live configuration.
	executor.suite, executor.guest = "", ""
	if err := fixture.scheduler.requirePublicationTestPolicy(ctx, fixture.repo, publication); err == nil || !strings.Contains(err.Error(), "not operator-approved") {
		t.Fatalf("promotion did not recheck live recipes: %v", err)
	}
	if err := fixture.scheduler.recoverPendingPublications(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := fixture.store.Publication(ctx, publication.ID)
	if err != nil || got.Status != model.PublicationFailed || len(fixture.git.promotions) != 0 || len(fixture.git.syncedBranches) != 1 {
		t.Fatalf("recovery bypassed revoked rotation: %#v, %v", got, err)
	}
}
