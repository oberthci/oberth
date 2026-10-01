package gitcache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGitCommandsNeverDetachBackgroundMaintenance pins the invariant that no
// Git command this package runs may spawn a detached background maintenance
// process.
//
// A detached child calls setsid(), leaves the command's process group, and is
// reparented to PID 1. Oberth is PID 1 in its Pod and reaps only the processes
// os/exec started for it, so each detached descendant becomes a permanent
// zombie. The push leg matters most: Git clears GIT_CONFIG_COUNT from the
// environment of the git-receive-pack transport child, so an environment-only
// override silently misses it and only the pinned global config file survives.
func TestGitCommandsNeverDetachBackgroundMaintenance(t *testing.T) {
	root := t.TempDir()
	trace := filepath.Join(root, "git-trace.log")
	cache, err := New(Config{
		Root:           filepath.Join(root, "cache"),
		CommandTimeout: 2 * time.Minute,
		Upstream:       func(string) (string, error) { return "", errors.New("upstream unused") },
		Env:            map[string]string{"GIT_TRACE": trace},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	run := func(spec commandSpec) {
		t.Helper()
		if err := cache.run(ctx, spec); err != nil {
			t.Fatalf("git %q: %v", spec.args, err)
		}
	}
	identity := []string{"-c", "user.name=Oberth", "-c", "user.email=oberth@localhost"}

	bare := filepath.Join(root, "dest.git")
	source := filepath.Join(root, "source")
	run(commandSpec{args: []string{"init", "--quiet", "--bare", bare}})
	run(commandSpec{args: []string{"init", "--quiet", source}})
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(commandSpec{dir: source, args: append(append([]string{}, identity...), "add", "file")})
	run(commandSpec{dir: source, args: append(append([]string{}, identity...), "commit", "--quiet", "-m", "seed")})

	// push spawns git-receive-pack as a transport child; clone and fetch run
	// auto maintenance in the command Oberth started directly.
	run(commandSpec{dir: source, args: []string{"push", "--quiet", bare, "HEAD:refs/heads/main"}})
	work := filepath.Join(root, "work")
	run(commandSpec{args: []string{"clone", "--quiet", "--no-checkout", "--no-hardlinks", bare, work}})
	run(commandSpec{dir: work, args: []string{"fetch", "--quiet", "--force", "origin"}})

	body, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read GIT_TRACE output: %v", err)
	}
	var maintenance, detached []string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "maintenance") || !strings.Contains(line, "run_command") {
			continue
		}
		maintenance = append(maintenance, line)
		for _, field := range strings.Fields(line) {
			if field == "--detach" {
				detached = append(detached, line)
			}
		}
	}
	if len(maintenance) == 0 {
		t.Fatalf("no auto maintenance observed in GIT_TRACE; the invariant was not exercised\n%s", body)
	}
	if len(detached) != 0 {
		t.Fatalf("git spawned %d detached maintenance process(es); each one orphans to PID 1 and leaks a zombie:\n%s",
			len(detached), strings.Join(detached, "\n"))
	}
}

// TestCommandEnvPinsManagedGlobalGitConfig proves the mechanism itself, so a
// future Git release that stops emitting the flag cannot make the behavioural
// test above pass vacuously.
func TestCommandEnvPinsManagedGlobalGitConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cache, err := New(Config{
		Root:     root,
		Upstream: func(string) (string, error) { return "", errors.New("upstream unused") },
	})
	if err != nil {
		t.Fatal(err)
	}

	managed := filepath.Join(root, managedGitConfigFile)
	body, err := os.ReadFile(managed)
	if err != nil {
		t.Fatalf("managed Git config was not written: %v", err)
	}
	for _, want := range []string{"[gc]", "[maintenance]", "autoDetach = false"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("managed Git config is missing %q:\n%s", want, body)
		}
	}

	// A caller-supplied layer must not be able to unpin the managed config or
	// re-enable detachment, exactly as with GIT_NO_REPLACE_OBJECTS.
	lookup := func(env []string, key string) string {
		t.Helper()
		for _, entry := range env {
			if name, value, ok := strings.Cut(entry, "="); ok && name == key {
				return value
			}
		}
		return ""
	}
	hostile := map[string]string{
		"GIT_CONFIG_GLOBAL":  "/tmp/attacker.gitconfig",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "gc.autodetach",
		"GIT_CONFIG_VALUE_0": "true",
	}
	for _, env := range [][]string{cache.commandEnv(nil), cache.commandEnv(hostile)} {
		if got := lookup(env, "GIT_CONFIG_GLOBAL"); got != managed {
			t.Fatalf("GIT_CONFIG_GLOBAL = %q, want the managed file %q", got, managed)
		}
		if got := lookup(env, "GIT_CONFIG_VALUE_0"); got != "false" {
			t.Fatalf("GIT_CONFIG_VALUE_0 = %q, want false", got)
		}
		if got := lookup(env, "GIT_CONFIG_KEY_0"); got != "gc.autodetach" {
			t.Fatalf("GIT_CONFIG_KEY_0 = %q, want gc.autodetach", got)
		}
	}
}

// TestCaptureBoundsDescendantHoldingOutputPipe covers the other way a Git
// command can outlive its process: the process exits but a descendant that
// escaped the process group still holds the stdout pipe, so Cmd.Wait blocks on
// the copying goroutine forever while the caller keeps the repository lock.
func TestCaptureBoundsDescendantHoldingOutputPipe(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is required to hold an inherited descriptor open")
	}
	cache, err := New(Config{
		Root:      t.TempDir(),
		GitBinary: "/bin/sh",
		Upstream:  func(string) (string, error) { return "", errors.New("upstream unused") },
	})
	if err != nil {
		t.Fatal(err)
	}

	// The shell exits immediately; the backgrounded child keeps the inherited
	// stdout pipe open well past the wait delay and then retires on its own.
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, waitErr := cache.capture(context.Background(), "", "-c", "sleep 20 & exit 0")
		done <- waitErr
	}()

	select {
	case waitErr := <-done:
		if !errors.Is(waitErr, exec.ErrWaitDelay) {
			t.Fatalf("capture error = %v, want exec.ErrWaitDelay", waitErr)
		}
		if elapsed := time.Since(started); elapsed < captureWaitDelay {
			t.Fatalf("capture returned after %s, before the %s wait delay", elapsed, captureWaitDelay)
		}
	case <-time.After(captureWaitDelay + 30*time.Second):
		t.Fatal("capture never returned: an escaped descendant holding the output pipe wedges the repository lock")
	}
}
