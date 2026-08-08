package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

type stubUplinkAuthenticator struct {
	value model.AuthenticatedUplink
	err   error
}

func (stub stubUplinkAuthenticator) Authenticate(context.Context, string) (model.AuthenticatedUplink, error) {
	return stub.value, stub.err
}

func TestAuthenticatorAdapterReturnsBoundUplinkActor(t *testing.T) {
	authenticator, err := NewAuthenticator(stubUplinkAuthenticator{value: model.AuthenticatedUplink{
		Uplink: model.Uplink{Identity: "agent@tuxbox", Fingerprint: "SHA256:bound"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := authenticator.Authenticate(context.Background(), "oberth_token")
	if err != nil {
		t.Fatal(err)
	}
	if actor != (api.Actor{Identity: "agent@tuxbox", Fingerprint: "SHA256:bound"}) {
		t.Fatalf("actor = %#v", actor)
	}
}

type stubReleaseQueue struct {
	requests []ReleaseRequest
}

type stubCIQueue struct {
	requests []CIRequest
}

type stubReceiveRecorder struct {
	events []model.ReceiveEventSpec
}

func (recorder *stubReceiveRecorder) RecordReceiveEvent(_ context.Context, event model.ReceiveEventSpec) (bool, error) {
	recorder.events = append(recorder.events, event)
	return false, nil
}

func (queue *stubReleaseQueue) AdmitRelease(_ context.Context, request ReleaseRequest) (model.EnqueueRunResult, error) {
	queue.requests = append(queue.requests, request)
	return model.EnqueueRunResult{Run: model.Run{ID: "release-run", SHA: request.ObjectSHA, TestedSHA: request.CommitSHA}}, nil
}

func (queue *stubCIQueue) EnqueueCI(_ context.Context, request CIRequest) (model.EnqueueRunResult, error) {
	queue.requests = append(queue.requests, request)
	return model.EnqueueRunResult{Run: model.Run{ID: "ci-run", SHA: request.SHA}}, nil
}

type stubPushGit struct {
	peeled        gitcache.PeeledObject
	reachable     bool
	remoteSHA     string
	remoteExists  bool
	ensureCalls   int
	peelCalls     int
	ancestorCalls int
	admissionSHAs []string
	remoteCalls   int
}

func (git *stubPushGit) Ensure(context.Context, string) (gitcache.Repository, error) {
	git.ensureCalls++
	return gitcache.Repository{}, nil
}

func (git *stubPushGit) PeelObject(context.Context, string, string) (gitcache.PeeledObject, error) {
	git.peelCalls++
	return git.peeled, nil
}

func (git *stubPushGit) ReleaseReachable(_ context.Context, _, _, admissionSHA string) (bool, error) {
	git.ancestorCalls++
	git.admissionSHAs = append(git.admissionSHAs, admissionSHA)
	return git.reachable, nil
}

func (git *stubPushGit) RemoteRef(context.Context, string, string) (string, bool, error) {
	git.remoteCalls++
	return git.remoteSHA, git.remoteExists, nil
}

type stubPushRepositories struct {
	repository model.Repository
}

type stubRepositoryDiscoverer struct {
	spec model.RepositorySpec
}

func (discoverer stubRepositoryDiscoverer) DiscoverRepository(context.Context, string) (model.RepositorySpec, error) {
	return discoverer.spec, nil
}

func (repositories stubPushRepositories) RepositoryByName(context.Context, string) (model.Repository, error) {
	if repositories.repository.ID == 0 {
		return model.Repository{}, errors.New("not found")
	}
	return repositories.repository, nil
}

func (repositories stubPushRepositories) CreateRepository(context.Context, model.RepositorySpec) (model.Repository, error) {
	return repositories.repository, nil
}

func (repositories stubPushRepositories) SetRepositoryDefaultBranch(_ context.Context, _ int64, branch string) (model.Repository, error) {
	value := repositories.repository
	value.DefaultBranch = branch
	return value, nil
}

func TestTagAdmissionRequiresFreshDefaultBranchReachability(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const admissionSHA = "cccccccccccccccccccccccccccccccccccccccc"
	repositories := stubPushRepositories{repository: model.Repository{ID: 7, Name: "oberth", UpstreamID: 1, DefaultBranch: "main"}}
	git := &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: commit, CommitSHA: commit}}
	releases := &stubReleaseQueue{}
	receives := &stubReceiveRecorder{}
	ingestor, err := NewPushIngestor(PushConfig{Repositories: repositories, Discoverer: stubRepositoryDiscoverer{spec: model.RepositorySpec{Name: "oberth", UpstreamID: 1, DefaultBranch: "main"}}, Git: git, Receives: receives, Releases: releases})
	if err != nil {
		t.Fatal(err)
	}

	unreachable, err := ingestor.Tag(context.Background(), TagPush{EventID: "receive-1", Repository: "oberth", Tag: "v1.2.3", NewOID: commit, Actor: "agent@tuxbox", ReleaseAdmissionSHA: admissionSHA})
	if err != nil {
		t.Fatal(err)
	}
	if unreachable.Admitted || len(releases.requests) != 0 {
		t.Fatalf("unreachable tag admitted: result=%#v requests=%d", unreachable, len(releases.requests))
	}

	git.reachable = true
	reachable, err := ingestor.Tag(context.Background(), TagPush{EventID: "receive-2", Repository: "oberth", Tag: "v1.2.3", NewOID: commit, Actor: "agent@tuxbox", ReleaseAdmissionSHA: admissionSHA})
	if err != nil {
		t.Fatal(err)
	}
	if !reachable.Admitted || len(releases.requests) != 1 || releases.requests[0].Tag != "v1.2.3" {
		t.Fatalf("reachable tag result=%#v requests=%#v", reachable, releases.requests)
	}

	git.remoteExists, git.remoteSHA = true, commit
	alreadyDelivered, err := ingestor.Tag(context.Background(), TagPush{
		EventID: "receive-3", Repository: "oberth", Tag: "v1.2.3", NewOID: commit, Actor: "agent@tuxbox", ReleaseAdmissionSHA: admissionSHA,
	})
	if err != nil || alreadyDelivered.Admitted || len(releases.requests) != 1 ||
		!strings.Contains(alreadyDelivered.Reason, "already delivered") {
		t.Fatalf("already-delivered tag result=%#v requests=%#v err=%v", alreadyDelivered, releases.requests, err)
	}
	git.remoteSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	conflict, err := ingestor.Tag(context.Background(), TagPush{
		EventID: "receive-4", Repository: "oberth", Tag: "v1.2.3", NewOID: commit, Actor: "agent@tuxbox", ReleaseAdmissionSHA: admissionSHA,
	})
	if err != nil || conflict.Admitted || len(releases.requests) != 1 ||
		!strings.Contains(conflict.Reason, "different object") {
		t.Fatalf("conflicting tag result=%#v requests=%#v err=%v", conflict, releases.requests, err)
	}
	if len(receives.events) != 3 || receives.events[1].Outcome != "release_delivered" || receives.events[2].Outcome != "release_rejected" {
		t.Fatalf("terminal tag receive outcomes = %#v", receives.events)
	}
	if git.peelCalls != 4 || git.ancestorCalls != 4 || git.remoteCalls != 3 {
		t.Fatalf("gate calls peel=%d ancestor=%d remote=%d", git.peelCalls, git.ancestorCalls, git.remoteCalls)
	}
	if len(git.admissionSHAs) != 4 {
		t.Fatalf("release admission calls = %#v", git.admissionSHAs)
	}
	for _, got := range git.admissionSHAs {
		if got != admissionSHA {
			t.Fatalf("release ancestry used snapshot %q, want %q", got, admissionSHA)
		}
	}
}

func TestDirectDefaultBranchReceiveUsesOrdinaryCIAdmission(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repositories := stubPushRepositories{repository: model.Repository{ID: 7, Name: "oberth", UpstreamID: 1, DefaultBranch: "main"}}
	git := &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: commit, CommitSHA: commit}}
	branches := &stubCIQueue{}
	receives := &stubReceiveRecorder{}
	ingestor, err := NewPushIngestor(PushConfig{
		Repositories: repositories, Discoverer: stubRepositoryDiscoverer{}, Git: git,
		Receives: receives, Branches: branches,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingestor.Branch(context.Background(), BranchPush{
		EventID: "receive-main", Repository: "oberth", Branch: "main",
		OldOID: commit, NewOID: commit, Actor: "agent@tuxbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run == nil || result.Run.ID != "ci-run" || len(branches.requests) != 1 || branches.requests[0].Branch != "main" || git.peelCalls != 1 {
		t.Fatalf("default branch result=%#v queue=%#v peel=%d", result, branches.requests, git.peelCalls)
	}
	if len(receives.events) != 0 {
		t.Fatalf("push ingestor wrote duplicate receive evidence = %#v", receives.events)
	}
}

type timeoutRunStore struct {
	run  model.Run
	repo model.Repository
}

func (store timeoutRunStore) RepositoryByName(context.Context, string) (model.Repository, error) {
	return store.repo, nil
}

func (store timeoutRunStore) ListRepositories(context.Context) ([]model.Repository, error) {
	return []model.Repository{store.repo}, nil
}

func (store timeoutRunStore) ResolveRun(context.Context, int64, string) (model.Run, error) {
	return store.run, nil
}

func TestWaitReturnsCleanTimeoutWithoutPolling(t *testing.T) {
	store := timeoutRunStore{
		repo: model.Repository{ID: 1, Name: "oberth"},
		run:  model.Run{ID: "run-1", RepoID: 1, Ref: "feature/a", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: model.RunRunning},
	}
	service, err := NewAPI(APIConfig{Runs: store, Signals: NewSignals(), MaximumWait: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@tuxbox"}, "wait", json.RawMessage(`{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(WaitResponse)
	if !ok || !response.StillRunning || response.Run.ID != "run-1" {
		t.Fatalf("wait response = %#v", value)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("wait elapsed = %s", elapsed)
	}
}

func TestSelectLogRangeRequiresOneUnambiguousNamedStep(t *testing.T) {
	t.Parallel()
	steps := []model.StepResult{
		{Burn: "test", Step: "unit"},
		{Burn: "test", Step: "integration"},
	}
	if burn, step, err := selectLogRange(steps, "unit"); err != nil || burn != "test" || step != "unit" {
		t.Fatalf("named step selection = %q/%q, %v", burn, step, err)
	}
	steps = append(steps, model.StepResult{Burn: "release", Step: "unit"})
	if _, _, err := selectLogRange(steps, "unit"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ambiguous step error = %v, want ErrInvalidInput", err)
	}
	if _, _, err := selectLogRange(steps, "missing"); err == nil {
		t.Fatal("unknown step selection unexpectedly succeeded")
	}
}

func TestRunToolsAcceptRepositoryDisambiguator(t *testing.T) {
	t.Parallel()
	store := timeoutRunStore{
		repo: model.Repository{ID: 1, Name: "oberth"},
		run: model.Run{ID: "run-1", RepoID: 1, RefKind: model.RefBranch, Ref: "feature/fab",
			SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TestedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: model.RunPassed},
	}
	control, err := NewAPI(APIConfig{Runs: store, Signals: NewSignals()})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "agent@host"}
	requests := map[string]string{
		"status":  `{"repo":"oberth","ref":"feature/fab"}`,
		"logs":    `{"repo":"oberth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","step":"test"}`,
		"wait":    `{"repo":"oberth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		"sync":    `{"repo":"oberth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		"promote": `{"repo":"oberth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","branch":"main"}`,
	}
	for name, arguments := range requests {
		if _, err := control.CallTool(context.Background(), actor, name, json.RawMessage(arguments)); err != nil && strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s rejected repository disambiguator: %v", name, err)
		}
	}
}

func TestWaitRejectsTimeoutAboveAdvertisedMaximum(t *testing.T) {
	t.Parallel()
	store := timeoutRunStore{
		repo: model.Repository{ID: 1, Name: "oberth"},
		run:  model.Run{ID: "run-1", RepoID: 1, Ref: "feature/a", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: model.RunRunning},
	}
	control, err := NewAPI(APIConfig{Runs: store, Signals: NewSignals(), MaximumWait: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "wait", json.RawMessage(`{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","timeout":121}`))
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("oversized timeout error = %v, want explicit ErrInvalidInput", err)
	}
}

func TestAuditGateBlocksMCPMutationAndSchedulerClaim(t *testing.T) {
	t.Parallel()
	gate := func(context.Context) error { return errors.New("external witness unavailable") }
	control := &API{mutationGate: gate}
	if _, err := control.CallTool(
		context.Background(), api.Actor{Identity: "agent@host"}, "issue_create",
		json.RawMessage(`{"title":"blocked","text":"audit unavailable"}`),
	); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mutating MCP tool error = %v, want ErrUnavailable", err)
	}

	scheduler := &Scheduler{mutationGate: gate}
	if err := scheduler.ProcessNext(context.Background()); !errors.Is(err, errMutationBlocked) {
		t.Fatalf("scheduler mutation gate error = %v, want errMutationBlocked", err)
	}
}
