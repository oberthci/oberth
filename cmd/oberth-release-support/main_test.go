package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testAccount = "0123456789abcdef0123456789abcdef"

func TestReleaseHeartbeatIsPeriodicBeforeCommandExit(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := newNotifyingBuffer()
	finished := make(chan error, 1)
	go func() {
		finished <- runHeartbeatCommand("publish-r2", exec.CommandContext(ctx, "/bin/sleep", "30"), output, 10*time.Millisecond)
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for strings.Count(output.String(), "release heartbeat: publish-r2 active\n") < 2 {
		select {
		case <-output.changed:
		case err := <-finished:
			t.Fatalf("heartbeat command exited before two progress records: %v; output=%q", err, output.String())
		case <-deadline.C:
			cancel()
			<-finished
			t.Fatalf("periodic heartbeat was not observable before command exit: %q", output.String())
		}
	}
	select {
	case err := <-finished:
		t.Fatalf("blocked command exited before cancellation: %v", err)
	default:
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("canceled heartbeat command returned no error")
	}
}

func TestReleaseHeartbeatPreservesChildExitStatus(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	err := runHeartbeatCommand("publish-r2", exec.Command("/bin/sh", "-c", "exit 37"), io.Discard, time.Hour)
	if got := releaseCommandExitCode(err); got != 37 {
		t.Fatalf("heartbeat child exit = %d, want 37; error=%v", got, err)
	}
}

func TestReleaseHeartbeatCLIPropagatesChildExitStatus(t *testing.T) {
	const helperEnvironment = "OBERTH_RELEASE_SUPPORT_TEST_MAIN"
	if os.Getenv(helperEnvironment) == "1" {
		os.Args = []string{"oberth-release-support", "heartbeat", "publish-r2", repositoryReleaseScript, "build"}
		main()
		return
	}

	fixture := t.TempDir()
	if err := os.Mkdir(filepath.Join(fixture, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, repositoryReleaseScript), []byte("#!/bin/sh\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(testBinary, "-test.run=^TestReleaseHeartbeatCLIPropagatesChildExitStatus$")
	command.Dir = fixture
	command.Env = append(os.Environ(), helperEnvironment+"=1", runnerOwnsGroupEnv+"=")
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 37 {
		t.Fatalf("CLI exit = %v, want 37; output=%q", err, output)
	}
}

func TestReleaseHeartbeatRejectsLogInjectionBeforeExecution(t *testing.T) {
	fixture := t.TempDir()
	if err := os.Mkdir(filepath.Join(fixture, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture, "executed")
	script := "#!/bin/sh\n: > \"$OBERTH_TEST_EXECUTION_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(fixture, repositoryReleaseScript), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture)
	t.Setenv("OBERTH_TEST_EXECUTION_MARKER", marker)
	var output bytes.Buffer
	err := runReleaseHeartbeat(context.Background(), "bad\nlabel", repositoryReleaseScript, []string{"build"}, &output, time.Hour)
	if err == nil || output.Len() != 0 {
		t.Fatalf("log-injecting heartbeat label result = %v, output=%q", err, output.String())
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("invalid heartbeat label executed the child: %v", statErr)
	}
}

func TestReleaseHeartbeatNeverSplitsAChildPartialLine(t *testing.T) {
	var output bytes.Buffer
	writer := &synchronizedWriter{output: &output}
	if _, err := writer.Write([]byte("partial-secret")); err != nil {
		t.Fatal(err)
	}
	if emitted, err := writer.heartbeat("publish-r2"); err != nil || !emitted {
		t.Fatalf("heartbeat inside partial line = %v, %v", emitted, err)
	}
	if got, want := output.String(), "release heartbeat: publish-r2 active\n"; got != want {
		t.Fatalf("output while child line is partial = %q, want %q", got, want)
	}
	if _, err := writer.Write([]byte("-value\n")); err != nil {
		t.Fatal(err)
	}
	const want = "release heartbeat: publish-r2 active\npartial-secret-value\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestReleaseHeartbeatSurvivesPartialChildOutputThroughLineBuffer(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := newLineBufferedSink()
	finished := make(chan error, 1)
	go func() {
		command := exec.CommandContext(ctx, "/bin/sh", "-c", "printf partial-secret; exec sleep 30")
		finished <- runHeartbeatCommand("publish-r2", command, output, 10*time.Millisecond)
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for strings.Count(output.String(), "release heartbeat: publish-r2 active\n") < 2 {
		select {
		case <-output.changed:
		case err := <-finished:
			t.Fatalf("partial-output command exited before two heartbeats: %v; output=%q", err, output.String())
		case <-deadline.C:
			cancel()
			<-finished
			t.Fatalf("partial child output starved periodic heartbeats: %q", output.String())
		}
	}
	if strings.Contains(output.String(), "partial-secret") {
		t.Fatalf("partial child record escaped before a safe boundary: %q", output.String())
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("canceled partial-output command returned no error")
	}
	output.Flush()
	if !strings.HasSuffix(output.String(), "partial-secret") {
		t.Fatalf("final child fragment was not flushed intact: %q", output.String())
	}
}

func TestReleaseHeartbeatPreservesBareCRSecretForDeployedMasker(t *testing.T) {
	const secret = "first-secret\rsecond-secret"
	output := &deployedMaskingSink{secret: []byte(secret)}
	writer := &synchronizedWriter{output: output}
	if _, err := writer.Write([]byte("first-secret\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.heartbeat("publish-r2"); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "release heartbeat: publish-r2 active\n"; got != want {
		t.Fatalf("deployed masker output while CR secret is partial = %q, want %q", got, want)
	}
	if _, err := writer.Write([]byte("second-secret\n")); err != nil {
		t.Fatal(err)
	}
	const want = "release heartbeat: publish-r2 active\n[MASKED]\n"
	if got := output.String(); got != want {
		t.Fatalf("deployed masker output = %q, want %q", got, want)
	}
}

func TestReleaseHeartbeatFailsClosedOnOversizedPartialLine(t *testing.T) {
	var output bytes.Buffer
	writer := &synchronizedWriter{output: &output}
	if _, err := writer.Write(bytes.Repeat([]byte("x"), maximumReleaseLineBytes+1)); err == nil {
		t.Fatal("oversized partial release output was accepted")
	}
	if _, err := writer.heartbeat("publish-r2"); err == nil {
		t.Fatal("heartbeat continued after unsafe child output")
	}
	if output.Len() != 0 {
		t.Fatalf("oversized partial output escaped: %q", output.String())
	}
}

func TestReleaseHeartbeatTerminatesActiveProcessTreeOnOutputFailure(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	output := &failPeriodicHeartbeatWriter{}
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; printf 'child-pid=%s\\n' \"$child\"; wait \"$child\"")
	started := time.Now()
	err := runHeartbeatCommand("publish-images", command, output, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "write periodic release heartbeat") {
		t.Fatalf("output failure result = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("release process tree termination took %s, want at most 3s", elapsed)
	}
	childPID := output.ChildPID(t)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitForProcessExit(childPID, 2*time.Second) {
		t.Fatalf("release descendant %d remained active after helper failure", childPID)
	}
}

func TestReleaseHeartbeatAtomicallyTerminatesIsolatedRunnerGroup(t *testing.T) {
	const helperEnvironment = "OBERTH_RELEASE_GROUP_TEST_HELPER"
	const markerEnvironment = "OBERTH_RELEASE_GROUP_TEST_MARKER"
	if os.Getenv(helperEnvironment) == "1" {
		output := &failPeriodicHeartbeatWriter{}
		command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; printf '%s\\n' \"$child\" > \"$OBERTH_RELEASE_GROUP_TEST_MARKER\"; printf 'child-pid=%s\\n' \"$child\"; wait \"$child\"")
		_ = runHeartbeatCommand("publish-images", command, output, 10*time.Millisecond)
		os.Exit(91)
	}

	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-pid")
	command := exec.Command(testBinary, "-test.run=^TestReleaseHeartbeatAtomicallyTerminatesIsolatedRunnerGroup$")
	command.Env = append(os.Environ(), helperEnvironment+"=1", markerEnvironment+"="+marker, runnerOwnsGroupEnv+"=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	})
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("isolated release process group was not terminated promptly")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("isolated helper exit = %v, want SIGKILL", waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("isolated helper status = %v, want SIGKILL", exitErr.Sys())
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || childPID <= 0 {
		t.Fatalf("release child PID marker = %q, %v", body, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitForProcessExit(childPID, 2*time.Second) {
		t.Fatalf("atomic group termination left release descendant %d active", childPID)
	}
}

func TestReleaseHeartbeatLocalLeaderCancellationKillsDescendants(t *testing.T) {
	const helperEnvironment = "OBERTH_RELEASE_LOCAL_LEADER_TEST_HELPER"
	const markerEnvironment = "OBERTH_RELEASE_LOCAL_LEADER_TEST_MARKER"
	if os.Getenv(helperEnvironment) == "1" {
		t.Setenv(runnerOwnsGroupEnv, "")
		ctx, cancel := context.WithCancel(context.Background())
		output := newNotifyingBuffer()
		finished := make(chan error, 1)
		go func() {
			command := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & child=$!; printf '%s\\n' \"$child\" > \"$OBERTH_RELEASE_LOCAL_LEADER_TEST_MARKER\"; printf 'child-ready\\n'; wait \"$child\"")
			finished <- runHeartbeatCommand("publish-images", command, output, 10*time.Millisecond)
		}()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for !strings.Contains(output.String(), "child-ready\n") {
			select {
			case <-output.changed:
			case err := <-finished:
				t.Fatalf("local leader child exited before cancellation: %v", err)
			case <-deadline.C:
				t.Fatal("local leader child did not become ready")
			}
		}
		cancel()
		if err := <-finished; err == nil {
			t.Fatal("local leader cancellation returned no error")
		}
		body, err := os.ReadFile(os.Getenv(markerEnvironment))
		if err != nil {
			t.Fatal(err)
		}
		childPID, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil || childPID <= 0 {
			t.Fatalf("local leader child PID marker = %q, %v", body, err)
		}
		if !waitForProcessExit(childPID, 2*time.Second) {
			t.Fatalf("local leader cancellation left descendant %d active", childPID)
		}
		return
	}

	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-pid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, testBinary, "-test.run=^TestReleaseHeartbeatLocalLeaderCancellationKillsDescendants$")
	command.Env = append(os.Environ(), helperEnvironment+"=1", markerEnvironment+"="+marker, runnerOwnsGroupEnv+"=")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("local process-group leader cancellation: %v\n%s", err, output)
	}
}

func TestReleaseHeartbeatWaitDelayTerminatesBackgroundDescendant(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	output := newNotifyingBuffer()
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; printf 'child-pid=%s\\n' \"$child\"")
	started := time.Now()
	err := runHeartbeatCommand("publish-images", command, output, time.Hour)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("background descendant result = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("background descendant cleanup took %s, want at most 2s", elapsed)
	}
	childPID := childPIDFromOutput(t, output.String())
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitForProcessExit(childPID, 2*time.Second) {
		t.Fatalf("WaitDelay cleanup left release descendant %d active", childPID)
	}
}

func TestReleaseHeartbeatNonzeroExitRetiresBackgroundDescendant(t *testing.T) {
	t.Setenv(runnerOwnsGroupEnv, "")
	output := newNotifyingBuffer()
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; printf 'child-pid=%s\\n' \"$child\"; exit 37")
	started := time.Now()
	err := runHeartbeatCommand("publish-images", command, output, time.Hour)
	if got := releaseCommandExitCode(err); got != 37 {
		t.Fatalf("nonzero background-parent exit = %d, want 37; error=%v", got, err)
	}
	if !strings.Contains(err.Error(), "release command left a background process") {
		t.Fatalf("nonzero background-parent error lacks containment evidence: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("nonzero background descendant cleanup took %s, want at most 2s", elapsed)
	}
	childPID := childPIDFromOutput(t, output.String())
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitForProcessExit(childPID, 2*time.Second) {
		t.Fatalf("nonzero exit left release descendant %d active", childPID)
	}
}

func TestReleaseActionCommandAllowsExactlyThePipelineActions(t *testing.T) {
	actions := []string{"build", "publish-r2", "publish-images", "package-chart", "publish-chart", "verify", "finalize"}
	for _, action := range actions {
		command, err := releaseActionCommand(context.Background(), action)
		if err != nil {
			t.Fatalf("release action %q: %v", action, err)
		}
		if command.Path != repositoryReleaseScript || len(command.Args) != 2 || command.Args[0] != repositoryReleaseScript || command.Args[1] != action {
			t.Fatalf("release action %q command = path %q args %v", action, command.Path, command.Args)
		}
	}
	if command, err := releaseActionCommand(context.Background(), "validate"); err == nil || command != nil {
		t.Fatalf("unknown heartbeat release action = %#v, %v", command, err)
	}
}

type notifyingBuffer struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	changed chan struct{}
}

type lineBufferedSink struct {
	mu      sync.Mutex
	pending bytes.Buffer
	visible bytes.Buffer
	changed chan struct{}
}

type deployedMaskingSink struct {
	mu      sync.Mutex
	secret  []byte
	pending bytes.Buffer
	visible bytes.Buffer
}

func (sink *deployedMaskingSink) Write(value []byte) (int, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	_, _ = sink.pending.Write(value)
	for {
		body := sink.pending.Bytes()
		boundary := bytes.IndexByte(body, '\n')
		if boundary < 0 {
			break
		}
		masked := bytes.ReplaceAll(body[:boundary], sink.secret, []byte("[MASKED]"))
		_, _ = sink.visible.Write(masked)
		_ = sink.visible.WriteByte('\n')
		sink.pending.Next(boundary + 1)
	}
	return len(value), nil
}

func (sink *deployedMaskingSink) String() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.visible.String()
}

type failPeriodicHeartbeatWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (writer *failPeriodicHeartbeatWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if bytes.HasPrefix(value, []byte("release heartbeat:")) && strings.Contains(writer.buffer.String(), "child-pid=") {
		return 0, io.ErrClosedPipe
	}
	return writer.buffer.Write(value)
}

func (writer *failPeriodicHeartbeatWriter) ChildPID(t *testing.T) int {
	t.Helper()
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return childPIDFromOutput(t, writer.buffer.String())
}

func childPIDFromOutput(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		value, found := strings.CutPrefix(line, "child-pid=")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(value)
		if err == nil && pid > 0 {
			return pid
		}
	}
	t.Fatalf("release child PID was not captured: %q", output)
	return 0
}

func processIsRunning(pid int) bool {
	// #nosec G304 -- pid comes from the local child process started by this test.
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	closeParen := bytes.LastIndexByte(body, ')')
	if closeParen < 0 {
		return true
	}
	fields := bytes.Fields(body[closeParen+1:])
	return len(fields) == 0 || string(fields[0]) != "Z"
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processIsRunning(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newLineBufferedSink() *lineBufferedSink {
	return &lineBufferedSink{changed: make(chan struct{}, 1)}
}

func (sink *lineBufferedSink) Write(value []byte) (int, error) {
	sink.mu.Lock()
	_, _ = sink.pending.Write(value)
	for {
		body := sink.pending.Bytes()
		boundary := bytes.IndexByte(body, '\n')
		if boundary < 0 {
			break
		}
		_, _ = sink.visible.Write(body[:boundary+1])
		sink.pending.Next(boundary + 1)
	}
	sink.mu.Unlock()
	select {
	case sink.changed <- struct{}{}:
	default:
	}
	return len(value), nil
}

func (sink *lineBufferedSink) Flush() {
	sink.mu.Lock()
	_, _ = sink.visible.Write(sink.pending.Bytes())
	sink.pending.Reset()
	sink.mu.Unlock()
}

func (sink *lineBufferedSink) String() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.visible.String()
}

func newNotifyingBuffer() *notifyingBuffer {
	return &notifyingBuffer{changed: make(chan struct{}, 1)}
}

func (buffer *notifyingBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	written, err := buffer.buffer.Write(value)
	buffer.mu.Unlock()
	select {
	case buffer.changed <- struct{}{}:
	default:
	}
	return written, err
}

func (buffer *notifyingBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestExchangeRequestsOnlyTheOberthPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer parent-token" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/user/tokens/verify":
			_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "result": map[string]any{"id": testAccount, "status": "active"}})
		case "/accounts/" + testAccount + "/r2/temp-access-credentials":
			var requestBody map[string]any
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			prefixes, ok := requestBody["prefixes"].([]any)
			if !ok || len(prefixes) != 1 || prefixes[0] != "oberth/" {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"accessKeyId": "access", "secretAccessKey": "secret", "sessionToken": "session"},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	issued, err := exchange(context.Background(), server.Client(), server.URL, "parent-token", testAccount, "cloudtaser-releases", "oberth/")
	if err != nil {
		t.Fatal(err)
	}
	if !validCredentials(issued) {
		t.Fatalf("issued credentials = %#v", issued)
	}
}

func TestSemverCompareAndPrivateCurlConfig(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"semver-compare", "v1.2.3-rc.1", "v1.2.3"}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "-1" {
		t.Fatalf("comparison = %q", output.String())
	}
	path := filepath.Join(t.TempDir(), "r2.conf")
	if err := writeCurlConfig(path, credentials{"access", "secret", "session"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("curl config mode = %o", info.Mode().Perm())
	}
}

func TestExchangeErrorNeverContainsParentToken(t *testing.T) {
	secret := "parent-token"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	_, err := exchange(context.Background(), server.Client(), server.URL, secret, testAccount, "cloudtaser-releases", "oberth/")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed token or was nil: %v", err)
	}
}

func TestNormalizeTreeUsesOneSourceTimestampAndRejectsLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "values.yaml")
	if err := os.WriteFile(file, []byte("image: exact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 6, 10, 30, 0, 0, time.UTC)
	if err := normalizeTree(root, created); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, root} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(created) {
			t.Fatalf("%s timestamp = %s, want %s", path, info.ModTime(), created)
		}
	}
	if err := os.Symlink(file, filepath.Join(root, "linked-values.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := normalizeTree(root, created); err == nil {
		t.Fatal("chart source containing a symlink was accepted")
	}
}

func TestInspectChartIndexDistinguishesAbsentExactAndConflict(t *testing.T) {
	digest := strings.Repeat("a", 64)
	chartURL := "https://charts.cloudtaser.io/oberth/oberth-1.2.3.tgz"
	path := filepath.Join(t.TempDir(), "index.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("apiVersion: v1\nentries: {}\n")
	if state, err := inspectChartIndex(path, "1.2.3", digest, chartURL); err != nil || state != "absent" {
		t.Fatalf("absent state = %q, %v", state, err)
	}
	write("apiVersion: v1\nentries:\n  oberth:\n  - version: 1.2.3\n    digest: " + digest + "\n    urls:\n    - " + chartURL + "\n")
	if state, err := inspectChartIndex(path, "1.2.3", digest, chartURL); err != nil || state != "exact" {
		t.Fatalf("exact state = %q, %v", state, err)
	}
	if _, err := inspectChartIndex(path, "1.2.3", strings.Repeat("b", 64), chartURL); err == nil {
		t.Fatal("conflicting chart digest was accepted")
	}
	if _, err := inspectChartIndex(path, "1.2.3", "sha256:"+digest, chartURL); err == nil {
		t.Fatal("OCI-style digest was accepted for a Helm index entry")
	}
}

func TestRegistryDigestRejectsCredentialExfiltrationTarget(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(key, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveChartDigest(context.Background(), "attacker.invalid/example/oberth:v1.2.3", key)
	if err == nil || !strings.Contains(err.Error(), "fixed Oberth chart repository") {
		t.Fatalf("foreign registry result = %v", err)
	}
}
