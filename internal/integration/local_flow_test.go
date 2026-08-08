package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/job"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const localPipeline = `//go:build ignore

package main

import "oberth"

var ReleaseSecrets = []string{"local-release"}

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	test := ctx.Go.Test("./...")
	test.Name = "go-test"
	return oberth.New().
		Retrograde("test",
			oberth.Step{Name: "checkout-sha", Command: "git", Args: []string{"rev-parse", "HEAD"}},
			test,
		).
		Prograde("build",
			oberth.Step{Name: "go-build", Command: "go", Args: []string{"build", "./..."}},
		).DependsOn("test").
		Release("release",
			oberth.Step{Name: "publish", Command: "sh", Args: []string{"-c", "printf '%s\\n' 'example-release-secret'"}},
		).DependsOn("build").
		Build()
}
`

const localReleaseSecret = "example-release-secret"

type localJobControl struct {
	workspaceRoot string
	mu            sync.Mutex
	requests      map[string]job.Request
	cancellations map[string]chan struct{}
	cancelled     map[string]bool
	blockSHA      string
	blockStarted  chan struct{}
	cancelEvents  chan string
	blockOnce     sync.Once
}

func (control *localJobControl) Create(_ context.Context, request job.Request) (string, error) {
	control.mu.Lock()
	defer control.mu.Unlock()
	if existing, ok := control.requests[request.JobName]; ok {
		if !reflect.DeepEqual(existing, request) {
			return "", fmt.Errorf("local Job %s already has a different intent", request.JobName)
		}
		return request.JobName, nil
	}
	control.requests[request.JobName] = request
	control.cancellations[request.JobName] = make(chan struct{})
	return request.JobName, nil
}

func (control *localJobControl) Wait(
	ctx context.Context,
	name, runID string,
	destination io.Writer,
	secretValues [][]byte,
	_ []byte,
) (job.Completion, error) {
	control.mu.Lock()
	request, ok := control.requests[name]
	cancelled := control.cancellations[name]
	control.mu.Unlock()
	if !ok || request.RunID != runID {
		return job.Completion{}, fmt.Errorf("local Job %s has no matching intent", name)
	}
	if request.SHA == control.blockSHA {
		control.blockOnce.Do(func() { close(control.blockStarted) })
		select {
		case <-ctx.Done():
			return job.Completion{}, ctx.Err()
		case <-cancelled:
			return job.Completion{}, context.Canceled
		}
	}
	sourceRoot := filepath.Join(control.workspaceRoot, runID, "src")
	source, err := os.ReadFile(filepath.Join(sourceRoot, ".oberth", "periapsis.go"))
	if err != nil {
		return job.Completion{}, fmt.Errorf("read local Periapsis source: %w", err)
	}
	trigger := periapsis.TriggerCI
	tag := ""
	if request.Release {
		trigger = periapsis.TriggerRelease
		tag = request.Ref
	}
	pipeline, err := periapsis.Interpret(string(source), periapsis.NewContext(request.Repo, request.Ref, request.SHA, tag))
	if err != nil {
		return job.Completion{}, fmt.Errorf("interpret local Periapsis source: %w", err)
	}
	secrets := make([]string, len(secretValues))
	for index := range secretValues {
		secrets[index] = string(secretValues[index])
	}
	var summaryBody bytes.Buffer
	summary, runErr := (runner.Runner{
		Trigger: trigger, StepTimeout: 30 * time.Second, Secrets: secrets, SummaryWriter: &summaryBody,
	}).Run(ctx, *pipeline, sourceRoot, destination)
	completion := job.Completion{
		JobName: name, PodName: "local-" + runID, Succeeded: runErr == nil,
		Summary: append(json.RawMessage(nil), summaryBody.Bytes()...), Reason: summary.Error,
	}
	if runErr != nil {
		completion.ExitCode = 1
	}
	return completion, nil
}

func (control *localJobControl) Cancel(_ context.Context, name, _ string) error {
	control.mu.Lock()
	defer control.mu.Unlock()
	stop, ok := control.cancellations[name]
	if !ok || control.cancelled[name] {
		return nil
	}
	control.cancelled[name] = true
	close(stop)
	select {
	case control.cancelEvents <- name:
	default:
	}
	return nil
}

func (*localJobControl) TerminalState(context.Context, string) (*job.Completion, error) {
	return nil, nil
}

func (*localJobControl) SecretSnapshot(_ context.Context, jobName string, requested []string) (job.SecretSnapshot, error) {
	if !slices.Equal(requested, []string{"local-release"}) {
		return job.SecretSnapshot{}, fmt.Errorf("unexpected local release Secret request %v", requested)
	}
	return job.SecretSnapshot{
		Name:   "local-release-" + jobName,
		Digest: strings.Repeat("a", 64),
		Mounts: job.ReleaseSecretMounts{Secrets: []job.ReleaseSecret{{Name: "local-release", Keys: []string{"token"}}}},
		Data:   map[string][]byte{"secret-00-token-00": []byte(localReleaseSecret)},
	}, nil
}

type localHealth struct{}

func (localHealth) Ready(ctx context.Context) error { return ctx.Err() }

func (localHealth) Status(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]string{"database": "ready", "vcs": "ready", "cluster": "local"}, nil
}

type capturingReceiveHandler struct {
	inner  gitcache.ReceiveHandler
	mu     sync.Mutex
	events []gitcache.ReceiveEvent
}

func (handler *capturingReceiveHandler) HandleReceive(ctx context.Context, event gitcache.ReceiveEvent) error {
	if err := handler.inner.HandleReceive(ctx, event); err != nil {
		return err
	}
	handler.mu.Lock()
	handler.events = append(handler.events, event)
	handler.mu.Unlock()
	return nil
}

func (handler *capturingReceiveHandler) Events() []gitcache.ReceiveEvent {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]gitcache.ReceiveEvent(nil), handler.events...)
}

type mcpEnvelope struct {
	Result struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestLocalFABExampleFlow(t *testing.T) {
	t.Setenv("GOWORK", "off")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(goBinary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "space bearing root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	upstreamRoot := filepath.Join(root, "upstream")
	upstreamRepo := filepath.Join(upstreamRoot, "demo.git")
	work := filepath.Join(root, "source")
	dataRoot := filepath.Join(root, "data")
	for _, directory := range []string{upstreamRoot, work, dataRoot, filepath.Join(work, ".oberth")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "", nil, "init", "--bare", "--initial-branch=main", upstreamRepo)
	runGit(t, work, nil, "init", "--initial-branch=main")
	runGit(t, work, nil, "config", "user.name", "Oberth local flow")
	runGit(t, work, nil, "config", "user.email", "oberth-local@example.invalid")
	writeFile(t, filepath.Join(work, "go.mod"), "module example.invalid/demo\n\ngo 1.26.0\n")
	writeFile(t, filepath.Join(work, "answer.go"), answerSource(42))
	writeFile(t, filepath.Join(work, "answer_test.go"), `package demo

import "testing"

func TestAnswer(t *testing.T) {
	if Answer() != 42 {
		t.Fatalf("Answer() = %d, want 42", Answer())
	}
}
`)
	writeFile(t, filepath.Join(work, ".oberth", "periapsis.go"), localPipeline)
	runGit(t, work, nil, "add", ".")
	runGit(t, work, nil, "commit", "-m", "seed main")
	mainSHA := runGit(t, work, nil, "rev-parse", "HEAD")
	runGit(t, work, nil, "remote", "add", "upstream", upstreamRepo)
	runGit(t, work, nil, "push", "upstream", "main")
	runGit(t, work, nil, "checkout", "-b", "feature/local-flow")
	writeFile(t, filepath.Join(work, "answer.go"), answerSource(39))
	runGit(t, work, nil, "commit", "-am", "superseded in flight")
	supersededSHA := runGit(t, work, nil, "rev-parse", "HEAD")

	database, err := store.Open(ctx, filepath.Join(dataRoot, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.RegisterUpstream(ctx, "bootstrap@local", model.UpstreamSpec{
		Name: "local", Kind: "local", BaseURL: upstreamRoot,
	}); err != nil {
		t.Fatal(err)
	}

	upstreams := app.Upstreams{Catalog: database}
	cache, err := gitcache.New(gitcache.Config{
		Root: filepath.Join(dataRoot, "git"), Upstream: upstreams.Remote, CommandTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(dataRoot, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(dataRoot, "work")
	localJobs := &localJobControl{
		workspaceRoot: workspaceRoot,
		requests:      make(map[string]job.Request),
		cancellations: make(map[string]chan struct{}),
		cancelled:     make(map[string]bool),
		blockSHA:      supersededSHA,
		blockStarted:  make(chan struct{}),
		cancelEvents:  make(chan string, 4),
	}
	jobAdapter, err := app.NewJobs(localJobs, workspaceRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	signals := service.NewSignals()
	scheduler, err := service.NewScheduler(service.SchedulerConfig{
		Store: database, Git: cache, Logs: logs, Jobs: jobAdapter, ReleaseJobs: jobAdapter,
		Auditor: database, Signals: signals, WorkspaceRoot: workspaceRoot, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	clientSigner, clientKeyPath := createClientSigner(t, root)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	var tokenOutput bytes.Buffer
	issuer := auth.Issuer{
		Tokens: database,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
		Bind: func(ctx context.Context, credential model.TokenCredential) error {
			_, err := database.RegisterUplink(ctx, "bootstrap@local", model.UplinkSpec{
				Fingerprint: fingerprint, Identity: "agent@local", TokenCredentialID: credential.ID,
			})
			return err
		},
	}
	if _, err := issuer.Issue(ctx, "agent@local", &tokenOutput); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(tokenOutput.String())
	if !strings.HasPrefix(token, "oberth_") {
		t.Fatal("token issuer did not emit the expected one-time credential")
	}

	pushes, err := service.NewPushIngestor(service.PushConfig{
		Repositories: database, Discoverer: upstreams, Git: cache, Receives: database,
		Branches: scheduler, Releases: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	receiveHandler, err := service.NewReceiveHandler(pushes)
	if err != nil {
		t.Fatal(err)
	}
	capturedReceives := &capturingReceiveHandler{inner: receiveHandler}
	hostSigner := newSigner(t)
	sshService, err := sshserver.New(sshserver.Config{
		HostSigner: hostSigner, Resolver: app.SSHIdentities{Uplinks: database}, Git: cache,
		OnUpdate: capturedReceives,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sshService.ReplayPending(ctx); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	knownHostsPath := filepath.Join(root, "known_hosts")
	writeFile(t, knownHostsPath, knownhosts.Line([]string{listener.Addr().String()}, hostSigner.PublicKey())+"\n")
	runCtx, cancelRun := context.WithCancel(ctx)
	schedulerDone := make(chan error, 1)
	sshDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Run(runCtx) }()
	go func() { sshDone <- sshService.Serve(runCtx, listener) }()
	t.Cleanup(func() {
		cancelRun()
		_ = listener.Close()
		for name, done := range map[string]<-chan error{"scheduler": schedulerDone, "SSH": sshDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s shutdown: %v", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s did not stop", name)
			}
		}
	})

	controlAPI, err := service.NewAPI(service.APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, PromotionRuns: database, Enqueues: scheduler, Git: cache,
		Logs: logs, Auditor: database, Health: localHealth{}, Signals: signals,
		MaximumWait: 75 * time.Millisecond, PromotionWorkspaceRoot: workspaceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: database})
	if err != nil {
		t.Fatal(err)
	}
	httpAPI, err := api.New(authenticator, controlAPI, controlAPI, "test")
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewUnstartedServer(httpAPI.Handler())
	tlsServer.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	tlsServer.StartTLS()
	t.Cleanup(tlsServer.Close)

	testSSHExecutable := buildPortableTestSSH(t, goBinary)
	sshCommand, err := app.GitSSHCommand(clientKeyPath, knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	sshCommand = replaceTestSSHExecutable(t, sshCommand, testSSHExecutable)
	remote := "ssh://git@" + listener.Addr().String() + "/demo.git"
	branch := "feature/local-flow"
	requireUnauthorizedHTTP(t, tlsServer, "invalid-token")
	requireUnauthorizedMCP(t, tlsServer, "invalid-token")
	unauthorizedRoot := filepath.Join(root, "unauthorized")
	if err := os.MkdirAll(unauthorizedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, unauthorizedKeyPath := createClientSigner(t, unauthorizedRoot)
	unauthorizedSSHCommand, err := app.GitSSHCommand(unauthorizedKeyPath, knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedSSHCommand = replaceTestSSHExecutable(t, unauthorizedSSHCommand, testSSHExecutable)
	runGitFailure(t, work, map[string]string{"GIT_SSH_COMMAND": unauthorizedSSHCommand},
		"push", "--force", remote, "HEAD:refs/heads/"+branch)
	if _, err := database.RepositoryByName(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unauthorized ingress repository lookup = %v, want not found", err)
	}
	assertNoPendingObligations(t, cache, database)

	clonePath := filepath.Join(root, "clone-via-oberth")
	runGit(t, "", map[string]string{"GIT_SSH_COMMAND": sshCommand}, "clone", "--branch", "main", remote, clonePath)
	if got := runGit(t, clonePath, nil, "rev-parse", "HEAD"); got != mainSHA {
		t.Fatalf("clone through Oberth resolved %q, want upstream main %q", got, mainSHA)
	}
	for name, ref := range map[string]string{
		"branch": "refs/heads/refs/rejected",
		"tag":    "refs/tags/-rejected",
	} {
		runGitFailure(t, work, map[string]string{"GIT_SSH_COMMAND": sshCommand}, "push", remote, mainSHA+":"+ref)
		if got := strings.TrimSpace(runGit(t, "", map[string]string{"GIT_SSH_COMMAND": sshCommand}, "ls-remote", remote, ref)); got != "" {
			t.Fatalf("invalid %s ref became clone-visible: %q", name, got)
		}
	}
	assertNoPendingObligations(t, cache, database)
	restartedCache, err := gitcache.New(gitcache.Config{
		Root: filepath.Join(dataRoot, "git"), Upstream: upstreams.Remote, CommandTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedCache.ReplayPending(ctx, capturedReceives); err != nil {
		t.Fatalf("startup replay after rejected public refs: %v", err)
	}

	pushBranch(t, work, remote, sshCommand, branch)
	select {
	case <-localJobs.blockStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded example run did not enter its Job")
	}
	var running service.StatusResponse
	callMCP(t, tlsServer, token, "status", map[string]any{"ref": branch}, &running)
	if running.SHA != supersededSHA || running.Status != "running" || running.RunID == "" {
		t.Fatalf("running branch status = %#v", running)
	}
	var timedOut service.WaitResponse
	callMCP(t, tlsServer, token, "wait", map[string]any{"sha": supersededSHA}, &timedOut)
	if !timedOut.StillRunning || timedOut.Status != "running" || timedOut.SHA != supersededSHA {
		t.Fatalf("clean wait timeout = %#v", timedOut)
	}

	writeFile(t, filepath.Join(work, "answer.go"), answerSource(41))
	runGit(t, work, nil, "commit", "-am", "first red")
	firstSHA := runGit(t, work, nil, "rev-parse", "HEAD")
	pushBranch(t, work, remote, sshCommand, branch)
	select {
	case name := <-localJobs.cancelEvents:
		if name == "" {
			t.Fatal("superseded example run cancellation had no Job identity")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new branch push did not cancel the in-flight Job")
	}
	interrupted := waitRun(t, tlsServer, token, supersededSHA)
	if interrupted.Status != "interrupted" || interrupted.SHA != supersededSHA || interrupted.Issue != nil {
		t.Fatalf("superseded run status = %#v", interrupted)
	}
	interruptedRecord, err := database.Run(ctx, interrupted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if interruptedRecord.SupersededBy == "" || !strings.Contains(interruptedRecord.Reason, "superseded") {
		t.Fatalf("superseded run evidence = %#v", interruptedRecord)
	}

	first := waitRun(t, tlsServer, token, firstSHA)
	if first.Status != "failed" || first.FailedStep != "go-test" || first.Issue == nil || first.Burns["test"] != "failed" {
		t.Fatalf("first red status = %#v", first)
	}
	firstIssue := *first.Issue
	assertRevisionLog(t, tlsServer, token, firstSHA)
	var firstLog service.LogResponse
	callMCP(t, tlsServer, token, "logs", map[string]any{"sha": firstSHA, "step": "go-test"}, &firstLog)
	if firstLog.Burn != "test" || firstLog.Step != "go-test" || !strings.Contains(firstLog.Output, "FAIL") ||
		strings.Contains(firstLog.Output, "[test/checkout-sha]") || !strings.Contains(firstLog.Output, "[test/go-test]") ||
		strings.Contains(firstLog.Output, "[build/") ||
		!strings.Contains(firstLog.Output, "Answer() = 41") || strings.Contains(firstLog.Output, "Answer() = 40") {
		t.Fatalf("first red bounded log = %#v", firstLog)
	}
	issues := getIssues(t, tlsServer, token, "demo", "open")
	if len(issues.Issues) != 1 || issues.Issues[0].ID != firstIssue || issues.Issues[0].Occurrences != 1 ||
		!strings.Contains(issues.Issues[0].Body, firstSHA) || !strings.Contains(issues.Issues[0].Body, "Answer() = 41") ||
		!strings.Contains(issues.Issues[0].Body, "full step log: logs "+firstSHA+" go-test") {
		t.Fatalf("issues after first red = %#v", issues)
	}
	var issueLock api.IssueLockResponse
	callMCP(t, tlsServer, token, "issue_lock", map[string]any{"id": firstIssue}, &issueLock)
	if issueLock.ID != firstIssue || issueLock.Owner != "agent@local" || issueLock.ExpiresAt == "" {
		t.Fatalf("actor-bound MCP issue lock = %#v", issueLock)
	}
	if remoteRef(t, upstreamRepo, branch) != "" || remoteRef(t, upstreamRepo, "main") != mainSHA {
		t.Fatal("red branch was published upstream")
	}
	assertRunActor(t, ctx, database, first.RunID, "agent@local")
	assertNoPendingObligations(t, cache, database)

	writeFile(t, filepath.Join(work, "answer.go"), answerSource(40))
	runGit(t, work, nil, "commit", "-am", "second red")
	secondSHA := runGit(t, work, nil, "rev-parse", "HEAD")
	pushBranch(t, work, remote, sshCommand, branch)
	second := waitRun(t, tlsServer, token, secondSHA)
	if second.Status != "failed" || second.Issue == nil || *second.Issue != firstIssue {
		t.Fatalf("second red status = %#v", second)
	}
	assertRevisionLog(t, tlsServer, token, secondSHA)
	issues = getIssues(t, tlsServer, token, "demo", "open")
	if len(issues.Issues) != 1 || issues.Issues[0].ID != firstIssue || issues.Issues[0].Occurrences != 2 ||
		issues.Issues[0].CIWorkID != second.RunID || !strings.Contains(issues.Issues[0].Body, secondSHA) ||
		!strings.Contains(issues.Issues[0].Body, "Answer() = 40") || strings.Contains(issues.Issues[0].Body, firstSHA) ||
		strings.Contains(issues.Issues[0].Body, "Answer() = 41") ||
		!strings.Contains(issues.Issues[0].Body, "full step log: logs "+secondSHA+" go-test") {
		t.Fatalf("deduplicated issue after second red = %#v", issues)
	}
	var historicalFirstLog, secondLog service.LogResponse
	callMCP(t, tlsServer, token, "logs", map[string]any{"sha": firstSHA, "step": "go-test"}, &historicalFirstLog)
	callMCP(t, tlsServer, token, "logs", map[string]any{"sha": secondSHA, "step": "go-test"}, &secondLog)
	if historicalFirstLog.Output != firstLog.Output || !strings.Contains(secondLog.Output, "Answer() = 40") ||
		strings.Contains(secondLog.Output, "Answer() = 41") {
		t.Fatalf("historical log separation = first %#v, second %#v", historicalFirstLog, secondLog)
	}
	if remoteRef(t, upstreamRepo, branch) != "" || remoteRef(t, upstreamRepo, "main") != mainSHA {
		t.Fatal("second red branch was published upstream")
	}
	var synced model.Run
	callMCP(t, tlsServer, token, "sync", map[string]any{"sha": secondSHA}, &synced)
	if synced.ID != second.RunID || synced.SHA != secondSHA || remoteRef(t, upstreamRepo, branch) != secondSHA {
		t.Fatalf("optional red sync = %#v, upstream=%q", synced, remoteRef(t, upstreamRepo, branch))
	}
	assertRunActor(t, ctx, database, second.RunID, "agent@local")
	assertNoPendingObligations(t, cache, database)

	writeFile(t, filepath.Join(work, "answer.go"), answerSource(42))
	runGit(t, work, nil, "commit", "-am", "green")
	greenSHA := runGit(t, work, nil, "rev-parse", "HEAD")
	pushBranch(t, work, remote, sshCommand, branch)
	green := waitRun(t, tlsServer, token, greenSHA)
	if green.Status != "delivered" || green.Issue != nil || green.Burns["test"] != "passed" ||
		green.Burns["build"] != "passed" || green.Burns["release"] != "" {
		t.Fatalf("green status = %#v", green)
	}
	assertRevisionLog(t, tlsServer, token, greenSHA)
	issues = getIssues(t, tlsServer, token, "demo", "closed")
	if len(issues.Issues) != 1 || issues.Issues[0].ID != firstIssue || issues.Issues[0].Occurrences != 2 ||
		issues.Issues[0].ClosedAt == nil || !strings.Contains(issues.Issues[0].Body, "resolved by "+greenSHA) {
		t.Fatalf("closed issue after green = %#v", issues)
	}
	if got := remoteRef(t, upstreamRepo, branch); got != greenSHA {
		t.Fatalf("published green branch = %q, want %q", got, greenSHA)
	}
	if got := remoteRef(t, upstreamRepo, "main"); got != mainSHA {
		t.Fatalf("default branch changed = %q, want %q", got, mainSHA)
	}
	assertRunActor(t, ctx, database, green.RunID, "agent@local")
	assertNoPendingObligations(t, cache, database)
	assertExactRuns(t, ctx, database, "demo", map[string]string{
		interrupted.RunID: supersededSHA, first.RunID: firstSHA, second.RunID: secondSHA, green.RunID: greenSHA,
	})
	events := capturedReceives.Events()
	if len(events) != 4 || !uniqueReceiveIDs(events) {
		t.Fatalf("accepted receive events = %#v", events)
	}
	if err := receiveHandler.HandleReceive(ctx, events[3]); err != nil {
		t.Fatalf("replay accepted receive event: %v", err)
	}
	assertExactRuns(t, ctx, database, "demo", map[string]string{
		interrupted.RunID: supersededSHA, first.RunID: firstSHA, second.RunID: secondSHA, green.RunID: greenSHA,
	})
	assertNoPendingObligations(t, cache, database)

	const unreachableTag = "v0.0.0-unreachable"
	runGit(t, work, nil, "tag", unreachableTag, greenSHA)
	runGitFailure(t, work, map[string]string{"GIT_SSH_COMMAND": sshCommand},
		"push", remote, "refs/tags/"+unreachableTag+":refs/tags/"+unreachableTag)
	cloneFacingTag := parseRemoteRef(t, runGit(t, "", map[string]string{"GIT_SSH_COMMAND": sshCommand},
		"ls-remote", "--tags", "--refs", remote, "refs/tags/"+unreachableTag))
	if cloneFacingTag != "" || remoteTagRef(t, upstreamRepo, unreachableTag) != "" || len(capturedReceives.Events()) != 4 {
		t.Fatalf("unreachable tag escaped admission: cache=%q upstream=%q events=%#v",
			cloneFacingTag, remoteTagRef(t, upstreamRepo, unreachableTag), capturedReceives.Events())
	}
	runGit(t, work, nil, "tag", "-d", unreachableTag)
	assertNoPendingObligations(t, cache, database)

	mainWork := filepath.Join(root, "concurrent-main")
	runGit(t, "", nil, "clone", upstreamRepo, mainWork)
	runGit(t, mainWork, nil, "config", "user.name", "Oberth target mover")
	runGit(t, mainWork, nil, "config", "user.email", "oberth-target@example.invalid")
	writeFile(t, filepath.Join(mainWork, "main-note.txt"), "target moved after feature CI\n")
	runGit(t, mainWork, nil, "add", "main-note.txt")
	runGit(t, mainWork, nil, "commit", "-m", "move target before promotion")
	divergedMainSHA := runGit(t, mainWork, nil, "rev-parse", "HEAD")
	runGit(t, mainWork, nil, "push", "origin", "main")

	var admitted api.PromoteResponse
	callMCP(t, tlsServer, token, "promote", map[string]any{"sha": greenSHA, "branch": "main"}, &admitted)
	if admitted.ID == "" {
		t.Fatal("promote did not return an append-only promotion ID")
	}
	promoted := waitPromotion(t, tlsServer, token, admitted.ID)
	if promoted.Status != model.PromotionPassed || promoted.SourceSHA != greenSHA ||
		promoted.PreviousSHA != divergedMainSHA || promoted.ResultSHA == "" || promoted.ResultSHA == greenSHA ||
		promoted.RunID == "" || remoteRef(t, upstreamRepo, "main") != promoted.ResultSHA {
		t.Fatalf("merged-tree promotion = %#v, upstream main=%q", promoted, remoteRef(t, upstreamRepo, "main"))
	}
	promotionRun, err := database.Run(ctx, promoted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if promotionRun.Trigger != "promotion" || promotionRun.Status != model.RunPassed ||
		promotionRun.TestedSHA != promoted.ResultSHA || promotionRun.BaseSHA != divergedMainSHA {
		t.Fatalf("promotion proof run = %#v", promotionRun)
	}
	var promotedStatus service.StatusResponse
	callMCP(t, tlsServer, token, "status", map[string]any{"ref": promoted.ResultSHA}, &promotedStatus)
	if promotedStatus.RunID != promoted.RunID || promotedStatus.Status != "delivered" ||
		promotedStatus.Burns["test"] != "passed" || promotedStatus.Burns["build"] != "passed" {
		t.Fatalf("promoted result status = %#v", promotedStatus)
	}
	assertNoPendingObligations(t, cache, database)

	fastForwardWork := filepath.Join(root, "fast-forward-source")
	runGit(t, "", nil, "clone", upstreamRepo, fastForwardWork)
	runGit(t, fastForwardWork, nil, "config", "user.name", "Oberth fast-forward source")
	runGit(t, fastForwardWork, nil, "config", "user.email", "oberth-fast-forward@example.invalid")
	runGit(t, fastForwardWork, nil, "checkout", "-b", "feature/fast-forward")
	writeFile(t, filepath.Join(fastForwardWork, "fast-forward.txt"), "fast-forward source\n")
	runGit(t, fastForwardWork, nil, "add", "fast-forward.txt")
	runGit(t, fastForwardWork, nil, "commit", "-m", "fast-forward source")
	fastForwardSHA := runGit(t, fastForwardWork, nil, "rev-parse", "HEAD")
	pushBranch(t, fastForwardWork, remote, sshCommand, "feature/fast-forward")
	fastForwardRun := waitRun(t, tlsServer, token, fastForwardSHA)
	if fastForwardRun.Status != "delivered" || fastForwardRun.Burns["test"] != "passed" {
		t.Fatalf("fast-forward source run = %#v", fastForwardRun)
	}
	var fastForwardAdmission api.PromoteResponse
	callMCP(t, tlsServer, token, "promote", map[string]any{"sha": fastForwardSHA, "branch": "main"}, &fastForwardAdmission)
	fastForwardPromotion := waitPromotion(t, tlsServer, token, fastForwardAdmission.ID)
	if fastForwardPromotion.Status != model.PromotionPassed || fastForwardPromotion.PreviousSHA != promoted.ResultSHA ||
		fastForwardPromotion.ResultSHA != fastForwardSHA || fastForwardPromotion.RunID != "" ||
		remoteRef(t, upstreamRepo, "main") != fastForwardSHA {
		t.Fatalf("fast-forward promotion = %#v, upstream main=%q", fastForwardPromotion, remoteRef(t, upstreamRepo, "main"))
	}
	assertNoPendingObligations(t, cache, database)

	runGit(t, fastForwardWork, nil, "checkout", "-b", "feature/already-contained", fastForwardSHA)
	writeFile(t, filepath.Join(fastForwardWork, "contained-source.txt"), "source later contained by target\n")
	runGit(t, fastForwardWork, nil, "add", "contained-source.txt")
	runGit(t, fastForwardWork, nil, "commit", "-m", "source later contained by target")
	containedSourceSHA := runGit(t, fastForwardWork, nil, "rev-parse", "HEAD")
	pushBranch(t, fastForwardWork, remote, sshCommand, "feature/already-contained")
	containedSourceRun := waitRun(t, tlsServer, token, containedSourceSHA)
	if containedSourceRun.Status != "delivered" || containedSourceRun.Burns["build"] != "passed" {
		t.Fatalf("already-contained source run = %#v", containedSourceRun)
	}

	containedTargetWork := filepath.Join(root, "already-contained-target")
	runGit(t, "", nil, "clone", upstreamRepo, containedTargetWork)
	runGit(t, containedTargetWork, nil, "config", "user.name", "Oberth containing target")
	runGit(t, containedTargetWork, nil, "config", "user.email", "oberth-containing-target@example.invalid")
	runGit(t, containedTargetWork, nil, "merge", "--ff-only", "origin/feature/already-contained")
	writeFile(t, filepath.Join(containedTargetWork, "target-after-source.txt"), "target advanced after including source\n")
	runGit(t, containedTargetWork, nil, "add", "target-after-source.txt")
	runGit(t, containedTargetWork, nil, "commit", "-m", "advance target after including source")
	containedTargetSHA := runGit(t, containedTargetWork, nil, "rev-parse", "HEAD")
	runGit(t, containedTargetWork, nil, "push", "origin", "main")

	var containedAdmission api.PromoteResponse
	callMCP(t, tlsServer, token, "promote", map[string]any{"sha": containedSourceSHA, "branch": "main"}, &containedAdmission)
	containedPromotion := waitPromotion(t, tlsServer, token, containedAdmission.ID)
	if containedPromotion.Status != model.PromotionPassed || containedPromotion.PreviousSHA != containedTargetSHA ||
		containedPromotion.ResultSHA != containedTargetSHA || containedPromotion.RunID == "" ||
		remoteRef(t, upstreamRepo, "main") != containedTargetSHA {
		t.Fatalf("already-contained target promotion = %#v, upstream main=%q", containedPromotion, remoteRef(t, upstreamRepo, "main"))
	}
	containedProof, err := database.Run(ctx, containedPromotion.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if containedProof.Trigger != "promotion" || containedProof.Status != model.RunPassed ||
		containedProof.TestedSHA != containedTargetSHA || containedProof.BaseSHA != containedTargetSHA {
		t.Fatalf("already-contained target proof = %#v", containedProof)
	}
	assertNoPendingObligations(t, cache, database)

	const releaseTag = "v0.0.1-example"
	runGit(t, work, nil, "fetch", "upstream", "main")
	runGit(t, work, nil, "tag", releaseTag, containedPromotion.ResultSHA)
	pushTag(t, work, remote, sshCommand, releaseTag)
	release := waitRun(t, tlsServer, token, containedPromotion.ResultSHA)
	if release.Status != "delivered" || release.RunID == containedPromotion.RunID || release.Ref != releaseTag ||
		len(release.Burns) != 1 || release.Burns["release"] != "passed" {
		t.Fatalf("release burn status = %#v", release)
	}
	var releaseLog service.LogResponse
	callMCP(t, tlsServer, token, "logs", map[string]any{"sha": containedPromotion.ResultSHA, "step": "publish"}, &releaseLog)
	if releaseLog.Burn != "release" || !strings.Contains(releaseLog.Output, "[release/publish]") ||
		strings.Contains(releaseLog.Output, localReleaseSecret) || !strings.Contains(releaseLog.Output, runner.MaskReplacement) ||
		strings.Contains(releaseLog.Output, "[test/") || strings.Contains(releaseLog.Output, "[build/") {
		t.Fatalf("bounded masked release log = %#v", releaseLog)
	}
	if got := remoteTagRef(t, upstreamRepo, releaseTag); got != containedPromotion.ResultSHA {
		t.Fatalf("published release tag = %q, want %q", got, containedPromotion.ResultSHA)
	}
	assertRunActor(t, ctx, database, release.RunID, "agent@local")
	events = capturedReceives.Events()
	if len(events) != 7 || !uniqueReceiveIDs(events) || events[6].Update.Kind() != "tag" || events[6].Update.Ref != "refs/tags/"+releaseTag {
		t.Fatalf("branch and release receive events = %#v", events)
	}
	for name, refspec := range map[string]string{
		"move":   "+" + promoted.ResultSHA + ":refs/tags/" + releaseTag,
		"delete": ":refs/tags/" + releaseTag,
	} {
		runGitFailure(t, work, map[string]string{"GIT_SSH_COMMAND": sshCommand}, "push", remote, refspec)
		cloneFacingTag = parseRemoteRef(t, runGit(t, "", map[string]string{"GIT_SSH_COMMAND": sshCommand},
			"ls-remote", "--tags", "--refs", remote, "refs/tags/"+releaseTag))
		if cloneFacingTag != containedPromotion.ResultSHA || remoteTagRef(t, upstreamRepo, releaseTag) != containedPromotion.ResultSHA {
			t.Fatalf("release tag after rejected %s: cache=%q upstream=%q", name, cloneFacingTag, remoteTagRef(t, upstreamRepo, releaseTag))
		}
	}
	if len(capturedReceives.Events()) != 7 {
		t.Fatalf("rejected tag mutations reached receive handler: %#v", capturedReceives.Events())
	}
	assertNoPendingObligations(t, cache, database)
}

func requireUnauthorizedHTTP(t *testing.T, server *httptest.Server, token string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/issues?repo=demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("invalid bearer transport status/TLS = %d/%v", response.StatusCode, response.TLS)
	}
}

func requireUnauthorizedMCP(t *testing.T, server *httptest.Server, token string) {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"main"}}}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("invalid MCP bearer transport status/TLS = %d/%v", response.StatusCode, response.TLS)
	}
}

func assertRevisionLog(t *testing.T, server *httptest.Server, token, sha string) {
	t.Helper()
	var response service.LogResponse
	callMCP(t, server, token, "logs", map[string]any{"sha": sha, "step": "checkout-sha"}, &response)
	if response.Burn != "test" || response.Step != "checkout-sha" ||
		!strings.Contains(response.Output, "[test/checkout-sha] "+sha+"\n") ||
		strings.Contains(response.Output, "[test/go-test]") || strings.Contains(response.Output, "[build/") {
		t.Fatalf("checkout revision log for %s = %#v", sha, response)
	}
}

func assertRunActor(t *testing.T, ctx context.Context, database *store.Store, runID, actor string) {
	t.Helper()
	run, err := database.Run(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Actor != actor {
		t.Fatalf("run %s actor = %q, want %q", runID, run.Actor, actor)
	}
}

func assertExactRuns(
	t *testing.T,
	ctx context.Context,
	database *store.Store,
	repositoryName string,
	want map[string]string,
) {
	t.Helper()
	repository, err := database.RepositoryByName(ctx, repositoryName)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := database.ListRecentRuns(ctx, model.RunListFilter{RepoID: repository.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != len(want) {
		t.Fatalf("run count = %d, want %d: %#v", len(runs), len(want), runs)
	}
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		sha, ok := want[run.ID]
		if !ok || run.SHA != sha || seen[run.ID] {
			t.Fatalf("unexpected or duplicate run = %#v, want %#v", run, want)
		}
		seen[run.ID] = true
	}
}

func assertNoPendingObligations(t *testing.T, cache *gitcache.Cache, database *store.Store) {
	t.Helper()
	receives, err := cache.PendingReceives()
	if err != nil {
		t.Fatal(err)
	}
	publications, err := database.PendingPublications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(receives) != 0 || len(publications) != 0 {
		t.Fatalf("pending obligations = receives %#v, publications %#v", receives, publications)
	}
}

func waitRun(t *testing.T, server *httptest.Server, token, selector string) service.WaitResponse {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var response service.WaitResponse
		callMCP(t, server, token, "wait", map[string]any{"sha": selector}, &response)
		if !response.StillRunning {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s was still running after the local-flow deadline", selector)
		}
	}
}

func waitPromotion(t *testing.T, server *httptest.Server, token, id string) model.Promotion {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var response service.PromotionWaitResponse
		callMCP(t, server, token, "promote_status", map[string]any{"id": id}, &response)
		if !response.StillRunning {
			return response.Promotion
		}
		if time.Now().After(deadline) {
			t.Fatalf("promotion %s was still running after the local-flow deadline", id)
		}
	}
}

func callMCP(t *testing.T, server *httptest.Server, token, name string, arguments any, destination any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("MCP transport status/TLS = %d/%v", response.StatusCode, response.TLS)
	}
	var envelope mcpEnvelope
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("MCP %s RPC error %d: %s", name, envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result.IsError {
		text := "tool failed"
		if len(envelope.Result.Content) > 0 {
			text = envelope.Result.Content[0].Text
		}
		t.Fatalf("MCP %s: %s", name, text)
	}
	if err := json.Unmarshal(envelope.Result.StructuredContent, destination); err != nil {
		t.Fatalf("decode MCP %s result: %v", name, err)
	}
}

func getIssues(t *testing.T, server *httptest.Server, token, repository, state string) model.IssuePage {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/issues?repo="+repository+"&kind=ci&state="+state, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("issues transport status/TLS = %d/%v", response.StatusCode, response.TLS)
	}
	var page model.IssuePage
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

func pushBranch(t *testing.T, work, remote, sshCommand, branch string) {
	t.Helper()
	runGit(t, work, map[string]string{"GIT_SSH_COMMAND": sshCommand},
		"push", "--force", remote, "HEAD:refs/heads/"+branch)
}

func pushTag(t *testing.T, work, remote, sshCommand, tag string) {
	t.Helper()
	runGit(t, work, map[string]string{"GIT_SSH_COMMAND": sshCommand},
		"push", remote, "refs/tags/"+tag+":refs/tags/"+tag)
}

func remoteRef(t *testing.T, repository, branch string) string {
	t.Helper()
	output := runGit(t, "", nil, "ls-remote", "--heads", repository, "refs/heads/"+branch)
	return parseRemoteRef(t, output)
}

func remoteTagRef(t *testing.T, repository, tag string) string {
	t.Helper()
	output := runGit(t, "", nil, "ls-remote", "--tags", "--refs", repository, "refs/tags/"+tag)
	return parseRemoteRef(t, output)
}

func parseRemoteRef(t *testing.T, output string) string {
	t.Helper()
	if output == "" {
		return ""
	}
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("ls-remote output = %q", output)
	}
	return fields[0]
}

func uniqueReceiveIDs(events []gitcache.ReceiveEvent) bool {
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		if event.ID == "" || seen[event.ID] {
			return false
		}
		seen[event.ID] = true
	}
	return true
}

func runGit(t *testing.T, directory string, extraEnv map[string]string, arguments ...string) string {
	t.Helper()
	command := gitCommand(directory, extraEnv, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func runGitFailure(t *testing.T, directory string, extraEnv map[string]string, arguments ...string) {
	t.Helper()
	command := gitCommand(directory, extraEnv, arguments...)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("git %s unexpectedly succeeded\n%s", strings.Join(arguments, " "), output)
	}
}

func gitCommand(directory string, extraEnv map[string]string, arguments ...string) *exec.Cmd {
	isolatedArguments := []string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}
	isolatedArguments = append(isolatedArguments, arguments...)
	command := exec.Command("git", isolatedArguments...)
	command.Dir = directory
	environment := make([]string, 0, len(os.Environ())+len(extraEnv)+3)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "GIT_") {
			environment = append(environment, value)
		}
	}
	environment = append(environment,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	)
	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+extraEnv[key])
	}
	command.Env = environment
	return command
}

func buildPortableTestSSH(t *testing.T, goBinary string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "gitssh")
	build := exec.Command(goBinary, "build", "-buildvcs=false", "-o", output, "./testdata/gitssh")
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if result, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build UID-independent test SSH client: %v\n%s", err, result)
	}
	return output
}

func replaceTestSSHExecutable(t *testing.T, command, executable string) string {
	t.Helper()
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	portable := strings.Replace(command, "'ssh'", quoted, 1)
	if portable == command {
		t.Fatal("test SSH command did not contain its expected executable")
	}
	return portable
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func answerSource(value int) string {
	return fmt.Sprintf("package demo\n\nfunc Answer() int { return %d }\n", value)
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func createClientSigner(t *testing.T, root string) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "oberth-local-flow")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "client-key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}
