package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/secretstore"
	"golang.org/x/crypto/ssh"
)

// runBinary dispatches helper executables before ordinary CLI parsing.
func (a application) runBinary(ctx context.Context, argv []string) int {
	if code, handled := a.helperEntry(ctx, argv); handled {
		return code
	}
	if code, handled := gitcache.ReceiveHookInvocation(ctx, argv, a.in, a.diagnostic); handled {
		return code
	}
	args := argv[1:]
	if filepath.Base(argv[0]) == "git-ssh" {
		return a.gitSSH(ctx, args)
	}
	return a.run(ctx, args)
}

func (a application) gitSSH(ctx context.Context, args []string) int {
	path := a.env(gitssh.ConfigEnvironment)
	if len(args) >= 2 && args[0] == "--config" {
		path, args = args[1], args[2:]
	}
	cfg, err := parseGitSSH(args)
	if err == nil && a.env(gitssh.ConfigEnvironment) != "" {
		cfg.ReadFile = func(file string, private bool) ([]byte, error) {
			// Persisted keys share the generated sidecar's directory. An external
			// identity is the configured, read-only forge-key mount.
			if private && filepath.Dir(file) != filepath.Dir(path) {
				return readMountedGitKey(file)
			}
			return readServerState(file)
		}
	}
	if err == nil {
		cfg, err = gitssh.Configure(path, cfg)
	}
	if err != nil {
		return a.failed(a.gitSSHProblem(err))
	}
	cfg.Now, cfg.Scratch = a.config.Time, filepath.Dir(path)
	err = a.streamGitSSH(ctx, cfg)
	if err == nil {
		return 0
	}
	var remote *ssh.ExitError
	if errors.As(err, &remote) {
		return remote.ExitStatus()
	}
	return a.failed(err)
}

func parseGitSSH(args []string) (gitssh.Config, error) {
	if len(args) > 0 && args[0] == "--pack" {
		var err error
		args, err = packSSHArguments(args[1:])
		if err != nil {
			return gitssh.Config{}, err
		}
	}
	return gitssh.Parse(args)
}

func packSSHArguments(args []string) ([]string, error) {
	if len(args) != 3 || args[1] != "git-upload-pack" && args[1] != "git-receive-pack" {
		return nil, flowError("input", nil)
	}
	if _, err := reponame.ParseRepository(strings.TrimPrefix(strings.TrimSuffix(args[2], ".git"), "/")); err != nil {
		return nil, err
	}
	return []string{args[0], args[1] + " " + shellArgument(args[2])}, nil
}

func (a application) streamGitSSH(ctx context.Context, cfg gitssh.Config) error {
	if a.env(gitssh.ConfigEnvironment) == "" {
		if err := client.CheckUplinkExpiry(cfg.Identity, a.config.Time()); err != nil {
			return err
		}
	}

	input, ok := a.in.(io.ReadCloser)
	if !ok {
		input = io.NopCloser(a.in)
	}
	return a.gitSSHProblem(gitssh.Run(ctx, cfg, gitssh.IO{Input: input, Output: a.out, Diagnostics: a.diagnostic}))
}

func (a application) gitSSHProblem(err error) error {
	if a.env(gitssh.ConfigEnvironment) == "" {
		return gitssh.PushDiagnostic(err)
	}
	return err
}

func (a application) helperEntry(ctx context.Context, argv []string) (int, bool) {
	args := argv[1:]
	if len(args) > 0 && args[0] == "internal-finalizers" {
		if err := releaseInstallationFinalizers(ctx, args[1:]); err != nil {
			_, _ = fmt.Fprintln(a.diagnostic, err)
			return 1, true
		}
		return 0, true
	}
	if len(args) > 0 && args[0] == "internal-install" {
		if len(args) != 3 || args[1] != "--to" {
			return a.usage([]string{"Provide the destination file with --to."}), true
		}
		if err := argojob.InstallExecutable(args[2]); err != nil {
			_, _ = fmt.Fprintln(a.diagnostic, "oberth install:", err)
			return 1, true
		}
		return 0, true
	}
	if filepath.Base(argv[0]) != "oberth-hold" {
		if len(args) == 0 || args[0] != "internal-hold" {
			return 0, false
		}
		args = args[1:]
	}
	return argojob.RunHold(ctx, args, argojob.HoldIO{In: a.in, Out: a.out, Err: a.diagnostic}), true
}

// Projected keys may be links and root-owned; other users must not read them.
func readMountedGitKey(path string) ([]byte, error) {
	return secretstore.ReadMountedFileChecked(path, 64<<10, "files.forge_key", func(info os.FileInfo) error {
		if info.Mode().Perm()&0004 == 0 {
			return nil
		}
		problem := publicerr.New(publicerr.Forbidden, "Git SSH key is readable by others.", "Remove read access for others; mounted keys may remain readable by the server's group.", nil)
		problem.Context = []string{"File: " + path}
		return problem
	})
}
