package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxCapturedOutput = 64 << 10

// maxRefsOutput is the upper bound on ref listing output accepted by listRefs.
// A single ref line is ~90 bytes (refname + space + 40-char SHA + newline);
// 16 MB accommodates ~180K refs with margin. Beyond that the upstream is
// pathological and unbounded allocation is not acceptable.
const maxRefsOutput = 16 << 20

// managedGitConfigFile is the only global Git configuration any cache command
// sees. It lives beside the bare repositories so it shares their lifetime.
const managedGitConfigFile = "oberth.gitconfig"

// managedGitConfig disables Git's background maintenance detachment.
//
// Both knobs default to true. With them enabled, "git clone", "git fetch" and
// "git receive-pack" end by spawning "git maintenance run --auto --detach",
// which calls daemonize(): it forks, the parent exits, and the child calls
// setsid(). The survivor therefore leaves this command's process group and is
// reparented to PID 1. Oberth *is* PID 1 in its Pod and reaps only the
// children os/exec knows about, so every one of those descendants stays a
// zombie for the life of the process. Turning detachment off keeps maintenance
// as an ordinary child that Cmd.Wait reaps, and — because the work now happens
// before Git exits — it also runs under the repository lock its caller holds
// instead of racing concurrent clones and pushes from outside it.
const managedGitConfig = `# Managed by Oberth. Generated file; edits are overwritten.
#
# Background maintenance must never detach: a detached child calls setsid(),
# escapes this command's process group, reparents to PID 1, and leaks as a
# zombie because os/exec only reaps processes it started.
[gc]
	autoDetach = false
[maintenance]
	autoDetach = false
[transfer]
	fsckObjects = true
[receive]
	fsckObjects = true
[fetch]
	fsckObjects = true
`

// captureWaitDelay bounds os/exec's wait for stdout/stderr descriptors held by
// a process that escaped the command's process group. It is applied only when
// the caller supplies no stdin, because Cmd.Wait cannot interrupt a blocked
// read on a caller-owned stdin reader; see execute.
const captureWaitDelay = 5 * time.Second

func writeManagedGitConfig(path string) error {
	existing, err := os.ReadFile(path) // #nosec G304 -- path is derived from the validated cache root.
	if err == nil && string(existing) == managedGitConfig {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(managedGitConfig), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

type commandSpec struct {
	dir    string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	env    map[string]string
}

func (c *Cache) run(ctx context.Context, spec commandSpec) error {
	_, err := c.execute(ctx, spec, false)
	return err
}

func (c *Cache) capture(ctx context.Context, dir string, args ...string) (string, error) {
	return c.execute(ctx, commandSpec{dir: dir, args: args}, true)
}

func (c *Cache) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// c.gitBinary is operator configuration and every argument is constructed
	// by the validated Git layer; no shell is involved. Cancellation below
	// terminates and joins the entire process group, which CommandContext cannot.
	cmd := exec.Command(c.gitBinary, spec.args...) //nolint:gosec,noctx
	cmd.Dir = spec.dir
	cmd.Stdin = spec.stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = c.commandEnv(spec.env)
	if spec.stdin == nil {
		// With no caller stdin, os/exec's only copying goroutines drain the
		// child's stdout/stderr pipes, so the sole reason Wait can outlive the
		// process is a descendant that escaped the process group and still
		// holds a write end. Bounding that is safe here. It is deliberately not
		// set for the Git smart-protocol services: their stdin is the live SSH
		// channel, Cmd.Wait cannot interrupt a blocked read on it, and expiring
		// the delay would turn a healthy upload-pack into ErrWaitDelay. Those
		// commands stay bounded by the SSH connection deadline instead.
		cmd.WaitDelay = captureWaitDelay
	}

	var output tailBuffer
	output.max = maxCapturedOutput
	// Always retain the tail of stderr, bounded and redacted, so a failure at
	// the upstream boundary carries the forge's own reason (URL, "repository
	// not found", "Permission denied (publickey)") instead of a bare "exit
	// status 128" (issue #212). In capture mode stderr is already folded into
	// output; in streaming mode it flows to the caller's writer (if any) and is
	// teed into this buffer for the error path only.
	var stderrTail tailBuffer
	stderrTail.max = maxCapturedOutput
	switch {
	case capture:
		// Both streams into one buffer; os/exec reuses a single copier because
		// Stdout == Stderr (identical writer), so there is no concurrent write.
		cmd.Stdout = &output
		cmd.Stderr = &output
	case spec.stderr == nil:
		cmd.Stdout = spec.stdout
		cmd.Stderr = &stderrTail
	case interfaceEqual(spec.stdout, spec.stderr):
		// The caller captures combined output in one writer (e.g. the
		// smart-protocol services and listRefs pass stdout==stderr). os/exec
		// only uses a single copier when Stdout and Stderr are the SAME writer
		// value; handing it two distinct writers over one buffer would race
		// (two copier goroutines, one target). Preserve the single-writer
		// identity by teeing BOTH streams through one combined writer that also
		// feeds the tail buffer for the error path.
		combined := io.MultiWriter(spec.stdout, &stderrTail)
		cmd.Stdout = combined
		cmd.Stderr = combined
	default:
		cmd.Stdout = spec.stdout
		cmd.Stderr = io.MultiWriter(spec.stderr, &stderrTail)
	}

	c.logger.Printf("git %s", c.redactedCommand(spec.args))
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start git command: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			detail := stderrTail.String()
			if capture {
				detail = output.String()
			}
			detail = c.redactOutput(detail)
			c.logger.Printf("git command failed: %v", err)
			if detail != "" {
				return "", fmt.Errorf("git command failed: %w: %s", err, detail)
			}
			return "", fmt.Errorf("git command failed: %w", err)
		}
		return output.String(), nil
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		<-done
		detail := stderrTail.String()
		if capture {
			detail = output.String()
		}
		if detail = c.redactOutput(detail); detail != "" {
			return "", fmt.Errorf("git command stopped: %w: %s", context.Cause(ctx), detail)
		}
		return "", fmt.Errorf("git command stopped: %w", context.Cause(ctx))
	}
}

// interfaceEqual reports whether two writers are the same value, guarding the
// comparison against a panic on non-comparable dynamic types (mirrors the
// os/exec check that decides whether Stdout and Stderr can share one copier).
// A non-comparable writer conservatively reports false.
func interfaceEqual(a, b io.Writer) (equal bool) {
	defer func() {
		if recover() != nil {
			equal = false
		}
	}()
	return a == b
}

// redactOutput masks configured secrets in captured git output before it is
// surfaced in an error. The tail buffers already bound length; this bounds
// content. It mirrors the secret masking in redactedCommand. SSH remote URLs
// carry no credentials, so the common failure reasons (forge messages, remote
// URL, auth denial) survive intact while any configured token is replaced.
func (c *Cache) redactOutput(value string) string {
	for _, secret := range c.redact {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	return value
}

func (c *Cache) commandEnv(extra map[string]string) []string {
	values := map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                os.Getenv("HOME"),
		"LANG":                "C",
		"LC_ALL":              "C",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ALLOW_PROTOCOL":  "ssh:https:file",
	}
	for key, value := range c.env {
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	// Replacement refs are client-controlled Git objects. They must never
	// alter the meaning of a commit in any server-side validation, checkout,
	// ancestry, merge, upload, or receive command. Set this after all supplied
	// environment layers so it cannot be overridden accidentally.
	values["GIT_NO_REPLACE_OBJECTS"] = "1"
	// Pin the global configuration to the file this package owns. Two things
	// depend on it, so do not fold these layers together:
	//
	//   - GIT_CONFIG_GLOBAL survives transport handoff. When Git spawns
	//     git-upload-pack or git-receive-pack for a clone, fetch, or push, it
	//     deliberately clears GIT_CONFIG_COUNT (and GIT_NO_REPLACE_OBJECTS)
	//     from the child's environment so local settings cannot leak to a
	//     remote. A config *file* is still read, so this is the only layer
	//     that reaches those children.
	//   - GIT_CONFIG_COUNT outranks any repository-local config, keeping
	//     directly invoked commands such as "gc --auto" non-detaching even if
	//     a repository config disagrees.
	//
	// Pinning the file also drops the ambient $HOME/.gitconfig dependency,
	// matching the GIT_CONFIG_NOSYSTEM=1 already set above.
	if c.globalConfig != "" {
		values["GIT_CONFIG_GLOBAL"] = c.globalConfig
		values["GIT_CONFIG_COUNT"] = "5"
		values["GIT_CONFIG_KEY_0"] = "gc.autodetach"
		values["GIT_CONFIG_VALUE_0"] = "false"
		values["GIT_CONFIG_KEY_1"] = "maintenance.autodetach"
		values["GIT_CONFIG_VALUE_1"] = "false"
		values["GIT_CONFIG_KEY_2"] = "transfer.fsckObjects"
		values["GIT_CONFIG_VALUE_2"] = "true"
		values["GIT_CONFIG_KEY_3"] = "receive.fsckObjects"
		values["GIT_CONFIG_VALUE_3"] = "true"
		values["GIT_CONFIG_KEY_4"] = "fetch.fsckObjects"
		values["GIT_CONFIG_VALUE_4"] = "true"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (c *Cache) redactedCommand(args []string) string {
	clean := make([]string, len(args))
	for index, arg := range args {
		clean[index] = redactURL(arg)
		for _, secret := range c.redact {
			if secret != "" {
				clean[index] = strings.ReplaceAll(clean[index], secret, "***")
			}
		}
	}
	return fmt.Sprintf("%q", clean)
}

func redactURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	if parsed.User != nil {
		parsed.User = url.User("***")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

type tailBuffer struct {
	data []byte
	max  int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	length := len(value)
	if length >= b.max {
		b.data = append(b.data[:0], value[length-b.max:]...)
		return length, nil
	}
	if excess := len(b.data) + length - b.max; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, value...)
	return length, nil
}

func (b *tailBuffer) String() string { return string(bytes.TrimSpace(b.data)) }

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func defaultTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return 10 * time.Minute
	}
	return value
}

// listRefsAtMost is a bounded variant of listRefs. It requests at most
// maximum+1 entries from git and returns an error if the actual count
// exceeds the limit, preventing unbounded work on pathological ref spaces.
// Unlike listRefs, this uses a direct buffer (not the 64 KB tail-buffer
// capture path) so it can handle several thousand refs without truncation.
func (c *Cache) listRefsAtMost(ctx context.Context, path string, maximum int, prefixes ...string) (map[string]string, error) {
	countArg := fmt.Sprintf("--count=%d", maximum+1)
	args := append([]string{"for-each-ref", countArg, "--format=%(refname) %(objectname)"}, prefixes...)
	var buf bytes.Buffer
	if err := c.run(ctx, commandSpec{dir: path, args: args, stdout: &buf, stderr: &buf}); err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || ValidateSHA(parts[1]) != nil {
			return nil, fmt.Errorf("unexpected ref output %q", line)
		}
		if len(refs) >= maximum {
			return nil, fmt.Errorf("more than %d refs in a bounded namespace", maximum)
		}
		refs[parts[0]] = parts[1]
	}
	return refs, nil
}
