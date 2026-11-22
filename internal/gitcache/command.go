package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oberthci/oberth/internal/boundedio"
	"github.com/oberthci/oberth/internal/gitport"
)

type command struct {
	dir         string
	refWrites   []string
	refDeletes  bool
	args        []string
	input       io.Reader
	output      io.Writer
	remote      *Remote
	hooks       bool
	env         []string
	diagnostics io.Writer
	exchange    func(io.Writer, io.Reader) error
	progress    *commandProgress
	// sending, when set, is raised once the forge begins sending a pack.
	sending *atomic.Bool
}

func (c *Cache) capture(ctx context.Context, s command, limit int64) ([]byte, error) {
	b := boundedio.New(int(limit))
	s.output = b
	err := c.run(ctx, s)
	return bytes.TrimSpace(b.Bytes()), errors.Join(err, b.Err())
}
func (c *Cache) run(ctx context.Context, s command) error {
	total := c.budgets.local
	if transferCommand(s.args) {
		total = c.budgets.transfer
	}
	ctx, stopTotal := context.WithTimeout(ctx, total)
	defer stopTotal()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	progress := s.progress
	if progress == nil {
		// A transfer, and any command that waits on a forge, ends once
		// nothing has moved for the idle budget: a forge session that stalls
		// never holds its caller or its job for the whole total budget.
		idle := time.Duration(0)
		if transferCommand(s.args) || s.remote != nil {
			idle = c.budgets.idle
		}
		progress = watchProgress(cancel, idle)
		progress.moved = s.sending
	}
	defer progress.close()
	cmd, detail := c.nativeCommand(s, progress, cancel)
	owned := commandResources(s)
	var err error
	if s.exchange != nil {
		err = progress.exchange(ctx, cmd, s.exchange)
	} else {
		err = runOwned(ctx, cmd)
	}
	if err != nil {
		return c.commandFailure(s, detail, errors.Join(err, owned.clean()))
	}
	return nil
}
func (
	c *Cache,
) nativeCommand(
	s command,
	progress *commandProgress,
	cancel context.CancelCauseFunc,
) (
	*exec.Cmd,
	*nativeDetail,
) {
	hooks := "/dev/null"
	if s.hooks {
		hooks = filepath.Join(s.dir, "hooks")
	}
	args := append([]string{"-c", "core.hooksPath=" + hooks}, s.args...)
	cmd := exec.Command(c.config.Binary, args...)
	cmd.Dir = s.dir
	if s.input != nil {
		cmd.Stdin = commandReader{Reader: s.input, progress: progress}
	}
	output := s.output
	if output == nil {
		output = io.Discard
	}
	cmd.Stdout = commandWriter{Writer: output, progress: progress, cancel: cancel}
	cmd.Stderr = s.diagnostics
	detail := &nativeDetail{}
	if cmd.Stderr == nil {
		cmd.Stderr = detail
	}
	cmd.Stderr = commandWriter{Writer: cmd.Stderr, progress: progress, cancel: cancel}
	cmd.Env = c.commandEnv(s.remote)
	cmd.Env = append(cmd.Env, s.env...)
	cmd.Env = append(cmd.Env, c.objectEnv...)
	configureOwnedCommand(cmd)
	cmd.ExtraFiles = []*os.File{c.rootLock}
	cmd.WaitDelay = 5 * time.Second
	return cmd, detail
}
func (c *Cache) commandFailure(s command, detail *nativeDetail, err error) error {
	if message := strings.TrimSpace(detail.String()); message != "" {
		err = errors.Join(err, fmt.Errorf("%s", message))
	}
	if w, ok := s.output.(*limitedWriter); ok && w.err != nil {
		return errors.Join(w.err, unavailable("git", err))
	}
	if s.remote != nil {
		if c.config.Log != nil {
			c.config.Log(gitport.ForgeStatusReason, s.remote.repository+": "+gitport.ForgeOperatorRemedy+" "+err.Error())
		}
		return unavailable("forge", err)
	}
	return unavailable("git", err)
}
func (c *Cache) commandEnv(remote *Remote) []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(c.config.Root, "oberth.gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_LITERAL_PATHSPECS=1",
	}
	if remote != nil && remote.SSHBinary != "" {
		env = append(env, "GIT_SSH="+remote.SSHBinary, "GIT_SSH_VARIANT=simple", "OBERTH_GIT_SSH_CONFIG="+remote.SSHConfig)
	}
	return env
}

func localRefWriter(args []string) bool {
	operation := nativeOperation(args)
	return operation == "update-ref" || operation == "gc"
}

// Native progress must keep draining after the diagnostic prefix is full.
// Returning overflow here would stall the child instead of bounding detail.
type nativeDetail struct{ bytes.Buffer }

func (d *nativeDetail) Write(p []byte) (int, error) {
	n := len(p)
	left := 8192 - d.Len()
	if left > 0 {
		_, _ = d.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}
