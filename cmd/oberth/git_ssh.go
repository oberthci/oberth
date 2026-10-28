package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/app"

	"github.com/oberthci/oberth/internal/gitssh"
)

func runGitSSH(ctx context.Context, arguments []string, input io.ReadCloser, output, diagnostics io.Writer) int {
	done, err := bindGitSSHParent()
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, "oberth git-ssh: parent lifetime binding failed")
		return 255
	}
	defer done()
	err = gitssh.Run(ctx, arguments, os.Getenv(gitssh.ConfigEnvironment), input, output, diagnostics)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, "oberth git-ssh: transport refused or upstream Git service failed")
	}
	return gitssh.ExitCode(err)
}

// nativeGitSSHEnvironment keeps the existing generated host/identity policy,
// but gives Git an executable path instead of a shell-interpreted command.
func nativeGitSSHEnvironment(entries []app.SSHConfigEntry, options serveOptions, configPath string) (map[string]string, error) {
	body, err := app.BuildSSHConfig(entries, options.upstreamKey, options.knownHosts)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return nil, fmt.Errorf("SSH config path must be absolute and clean")
	}
	file, err := os.CreateTemp(filepath.Dir(configPath), ".oberth-git-ssh-")
	if err != nil {
		return nil, err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, configPath); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"GIT_SSH":         filepath.Join(filepath.Dir(executable), "oberth-git-ssh"),
		"GIT_SSH_VARIANT": "simple", gitssh.ConfigEnvironment: configPath,
	}, nil
}
