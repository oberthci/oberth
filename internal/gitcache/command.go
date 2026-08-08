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

	var output tailBuffer
	output.max = maxCapturedOutput
	if capture {
		cmd.Stdout = &output
		cmd.Stderr = &output
	} else {
		cmd.Stdout = spec.stdout
		cmd.Stderr = spec.stderr
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
			c.logger.Printf("git command failed: %v", err)
			return "", fmt.Errorf("git command failed: %w", err)
		}
		return output.String(), nil
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		<-done
		return "", fmt.Errorf("git command stopped: %w", context.Cause(ctx))
	}
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
