package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// runGitStdin runs one git command with the given stdin, failing the test on
// error. It exists beside runGit (gitcache_test.go) for the batch plumbing
// commands (mktree, update-ref --stdin) that read their input stream.
func runGitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Stdin = strings.NewReader(stdin)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// TestCaptureFailureIncludesGitStderr verifies that when a captured git
// command fails, the returned error carries git's own stderr (the reason)
// instead of a bare "exit status N" (issue #212). A failing cat-file exits 128
// and writes "fatal: Not a valid object name deadbeef" to stderr.
func TestCaptureFailureIncludesGitStderr(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)

	cache := newTestCache(t, "unused-upstream")
	_, err := cache.capture(context.Background(), bare, "cat-file", "-p", "deadbeef")
	if err == nil {
		t.Fatal("expected cat-file on a missing object to fail")
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("error must carry git stderr naming the bad object; got: %v", err)
	}
	if !strings.Contains(err.Error(), "git command failed") {
		t.Fatalf("error must keep the git-command-failed prefix; got: %v", err)
	}
}

// TestRunFailureIncludesGitStderrWithoutCallerWriter verifies the streaming
// (non-capture) path with no caller-supplied stderr writer still surfaces the
// reason: previously stderr was discarded entirely there.
func TestRunFailureIncludesGitStderrWithoutCallerWriter(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)

	cache := newTestCache(t, "unused-upstream")
	err := cache.run(context.Background(), commandSpec{dir: bare, args: []string{"cat-file", "-p", "deadbeef"}})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("streaming-mode error must carry git stderr; got: %v", err)
	}
}

// TestRunFailureTeesStderrToCallerAndError verifies the MultiWriter tee does
// not rob the caller: a supplied stderr writer still receives the full stream
// AND the error carries the reason.
func TestRunFailureTeesStderrToCallerAndError(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)

	cache := newTestCache(t, "unused-upstream")
	var callerStderr strings.Builder
	err := cache.run(context.Background(), commandSpec{
		dir: bare, args: []string{"cat-file", "-p", "deadbeef"}, stderr: &callerStderr,
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(callerStderr.String(), "deadbeef") {
		t.Fatalf("caller stderr writer must still receive the stream; got: %q", callerStderr.String())
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("error must also carry the reason; got: %v", err)
	}
}

// TestGitFailureDetailRedactsConfiguredSecret verifies the surfaced stderr is
// masked through the same secret set as command logging, so a configured token
// that appears in output cannot leak into an error string, CI issue, or log.
func TestGitFailureDetailRedactsConfiguredSecret(t *testing.T) {
	t.Parallel()
	cache, err := newBackendTestCache(t, Config{
		Root:           filepath.Join(t.TempDir(), "cache"),
		CommandTimeout: testtimeout.Start(10 * time.Second),
		Redact:         []string{"s3cr3t-token-value"},
		Upstream:       func(string) (string, error) { return "unused", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cache.redactOutput("fatal: auth failed for token s3cr3t-token-value at remote")
	if strings.Contains(got, "s3cr3t-token-value") {
		t.Fatalf("configured secret must be redacted from surfaced output; got: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("redaction marker missing; got: %q", got)
	}
}

// TestManagedGitConfigEnablesFsck verifies that the managed Git configuration
// enables fsck on transfer, receive, and fetch — ensuring that corrupt objects
// pushed or fetched into a cache are rejected at the transport boundary (#441).
func TestManagedGitConfigEnablesFsck(t *testing.T) {
	t.Parallel()
	for _, section := range []string{"transfer", "receive", "fetch"} {
		key := section + ".fsckObjects"
		if !strings.Contains(managedGitConfig, key+" = true") {
			// Try the alternate spacing (tab-indented in .gitconfig format).
			if !strings.Contains(managedGitConfig, "fsckObjects = true") {
				t.Fatalf("managedGitConfig must set %s = true", key)
			}
		}
	}
	// Verify the runtime environment carries the fsck overrides too.
	cache, err := newBackendTestCache(t, Config{
		Root:           t.TempDir(),
		CommandTimeout: testtimeout.Start(10 * time.Second),
		Upstream:       func(string) (string, error) { return "unused", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	env := cache.commandEnv(nil)
	envMap := make(map[string]string, len(env))
	for _, pair := range env {
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			envMap[pair[:idx]] = pair[idx+1:]
		}
	}
	wantKeys := map[string]string{
		"transfer.fsckObjects": "true",
		"receive.fsckObjects":  "true",
		"fetch.fsckObjects":    "true",
	}
	count, err := fmt.Sscan(envMap["GIT_CONFIG_COUNT"], new(int))
	if err != nil || count != 1 {
		t.Fatalf("GIT_CONFIG_COUNT parse: count=%d err=%v", count, err)
	}
	configCount := 0
	fmt.Sscan(envMap["GIT_CONFIG_COUNT"], &configCount)
	found := make(map[string]string)
	for i := 0; i < configCount; i++ {
		k := envMap[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		v := envMap[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]
		found[k] = v
	}
	for key, want := range wantKeys {
		if got := found[key]; got != want {
			t.Fatalf("GIT_CONFIG env %s = %q, want %q", key, got, want)
		}
	}
}

// TestListRefsSurvivesOutputBeyondCaptureTailLimit locks in the fix for the
// 64 KB capture truncation: listRefs previously went through the tail-ring
// capture path, so a ref listing larger than maxCapturedOutput silently lost
// its head — the first surviving line was usually a partial line that failed
// SHA validation, and on an exact line boundary refs vanished with no error
// at all. listRefs feeds upstream reconciliation and replacement-ref
// cleanup, so a truncated listing is an integrity bug, not cosmetics. The
// listing must come back complete through the direct buffer.
func TestListRefsSurvivesOutputBeyondCaptureTailLimit(t *testing.T) {
	t.Parallel()

	bare := filepath.Join(t.TempDir(), "many.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)
	tree := runGitStdin(t, bare, "", "mktree")
	commit := runGit(t, bare,
		"-c", "user.name=oberth-test", "-c", "user.email=test@oberth.ci",
		"commit-tree", tree, "-m", "seed")

	// ~69 bytes per ref line; 1500 refs is ~101 KB, comfortably past the
	// 64 KB tail-capture limit the old path truncated at, and nowhere near
	// the 16 MB maxRefsOutput refusal ceiling.
	const refCount = 1500
	var batch strings.Builder
	for i := 0; i < refCount; i++ {
		fmt.Fprintf(&batch, "create refs/heads/load/branch-%04d %s\n", i, commit)
	}
	runGitStdin(t, bare, batch.String(), "update-ref", "--stdin")

	cache := newTestCache(t, "unused-upstream")
	refs, err := cache.listRefs(context.Background(), bare, "refs/heads/")
	if err != nil {
		t.Fatalf("listRefs on a %d-ref repository: %v", refCount, err)
	}
	if len(refs) != refCount {
		t.Fatalf("listRefs returned %d refs, want %d — large listings must not truncate", len(refs), refCount)
	}
	for _, name := range []string{
		"refs/heads/load/branch-0000",
		fmt.Sprintf("refs/heads/load/branch-%04d", refCount-1),
	} {
		if refs[name] != commit {
			t.Fatalf("ref %s = %q, want %q", name, refs[name], commit)
		}
	}
}

func TestGitFailureLogNamesCommandAndRepository(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "private-value-repo.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)
	cache := newTestCache(t, "unused-upstream")
	var logs bytes.Buffer
	cache.logger = testLogger{&logs}
	cache.redact = append(cache.redact, "private-value")
	_, err := cache.capture(t.Context(), bare, "-c", "core.useReplaceRefs=false", "cat-file", "-p", "private-value")
	if err == nil {
		t.Fatal("missing object unexpectedly resolved")
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "git command failed:") {
			if !strings.Contains(line, `subcommand="cat-file"`) || !strings.Contains(line, cache.redactOutput(bare)) || strings.Contains(line, "private-value") {
				t.Fatalf("failure context missing or unredacted: %s", line)
			}
			return
		}
	}
	t.Fatal("missing command failure log")
}

func TestGitFailureLogWithoutWorkingDirectory(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused-upstream")
	var logs bytes.Buffer
	cache.logger = testLogger{&logs}
	missing := filepath.Join(t.TempDir(), "missing.git")
	_, err := cache.execute(t.Context(), commandSpec{repository: "forge/team/project", args: []string{"ls-remote", "--heads", missing}}, true)
	if err == nil {
		t.Fatal("missing repository unexpectedly resolved")
	}
	if !strings.Contains(logs.String(), `subcommand="ls-remote" repository="forge/team/project"`) {
		t.Fatalf("missing repository context: %s", logs.String())
	}
}

func TestGitLogsRedactUnlistedCredentials(t *testing.T) {
	t.Parallel()
	for _, config := range [][]string{
		{"-c", "http.extraHeader=Bearer unlisted-config-secret"},
		{"-chttp.extraHeader=Bearer unlisted-config-secret"},
	} {
		t.Run(config[0], func(t *testing.T) {
			bare := filepath.Join(t.TempDir(), "empty.git")
			runGit(t, "", "init", "--quiet", "--bare", bare)
			cache := newTestCache(t, "unused-upstream")
			cache.redact = nil
			var logs bytes.Buffer
			cache.logger = testLogger{&logs}
			_, err := cache.execute(t.Context(), commandSpec{
				dir:        bare,
				repository: "https://unlisted-user:unlisted-password@example.invalid/repo.git?unlisted-query#unlisted-fragment",
				args:       append(config, "cat-file", "-p", "missing-object"),
			}, true)
			if err == nil {
				t.Fatal("missing object unexpectedly resolved")
			}
			for _, secret := range []string{"unlisted-config-secret", "unlisted-user", "unlisted-password", "unlisted-query", "unlisted-fragment"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("synthetic credential %q leaked to logs", secret)
				}
			}
			for _, marker := range []string{`subcommand="cat-file"`, "example.invalid/repo.git"} {
				if !strings.Contains(logs.String(), marker) {
					t.Errorf("missing command context %q", marker)
				}
			}
		})
	}
}

func TestGitCommandTraceRedactsURLCredentialsWithoutConfiguredSecrets(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused-upstream")
	cache.redact = nil
	got := cache.redactedCommand([]string{"fetch", "https://unlisted-user:unlisted-password@example.invalid/repo.git?unlisted-query#unlisted-fragment"})
	if strings.Contains(got, "unlisted-") || !strings.Contains(got, "example.invalid/repo.git") {
		t.Fatal("URL trace leaked credentials or lost repository context")
	}
}

func TestGitSubcommandSkipsGlobalOptionValues(t *testing.T) {
	for _, option := range []string{"-c", "-C", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix", "--attr-source"} {
		t.Run(option, func(t *testing.T) {
			if got := gitSubcommand([]string{option, "private-value", "fetch", "origin"}); got != "fetch" {
				t.Fatalf("got %q", got)
			}
			if got := gitSubcommand([]string{option}); got != "" {
				t.Fatalf("missing value: %q", got)
			}
			if got := gitSubcommand([]string{option, "private-value"}); got != "" {
				t.Fatalf("missing command: %q", got)
			}
			if got := gitSubcommand([]string{option + "=private-value", "fetch"}); got != "fetch" {
				t.Fatalf("attached value: %q", got)
			}
		})
	}
	for _, args := range [][]string{
		{"--no-pager", "--namespace", "private", "-c", "key=value", "--work-tree=/tmp/work", "--git-dir", "/tmp/repo", "push", "origin"},
		{"-C/tmp/work", "-ckey=value", "push", "origin"},
		{"--literal-pathspecs", "--namespace=private", "push", "origin"},
		{"push", "--namespace", "argument"},
	} {
		if got := gitSubcommand(args); got != "push" {
			t.Fatalf("%q: got %q", args, got)
		}
	}
	if got := gitSubcommand(nil); got != "" {
		t.Fatalf("empty command: %q", got)
	}
}
