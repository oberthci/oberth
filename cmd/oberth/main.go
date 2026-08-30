package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/installer"
)

var errUsage = errors.New("usage error")

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		// Ctrl+C during interactive prompts: exit silently with the Unix
		// convention (128 + SIGINT signal number 2 = 130).
		if errors.Is(err, installer.ErrInterrupted) || errors.Is(err, app.ErrInterrupted) {
			_, _ = fmt.Fprintln(os.Stderr)
			os.Exit(130)
		}
		_, _ = fmt.Fprintln(os.Stderr, "oberth:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

const usageCommands = "audit, init, validate, install, upgrade, preload, serve, upstream, repo, schedules, fragments, artifacts, runs, run, log, repos, issues, status, uplink, access, secretstore, or version"

func runCLI(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: expected %s", errUsage, usageCommands)
	}
	switch arguments[0] {
	case "--help", "-h":
		_, err := fmt.Fprintf(output, "Usage: oberth <command> [flags]\n\nCommands: %s\n", usageCommands)
		return err
	case "audit":
		return runAudit(ctx, arguments[1:], output)
	case "init":
		return runInit(ctx, arguments[1:], output)
	case "validate":
		return runValidate(ctx, arguments[1:], output)
	case "serve":
		return runServe(ctx, arguments[1:], output)
	case "upstream":
		return runUpstream(ctx, arguments[1:], output)
	case "repo":
		return runRepo(ctx, arguments[1:], output)
	case "schedules":
		return runSchedules(ctx, arguments[1:], output)
	case "fragments":
		return runFragments(ctx, arguments[1:], output)
	case "artifacts":
		return runArtifacts(ctx, arguments[1:], output)
	case "runs":
		return runRuns(ctx, arguments[1:], output)
	case "run":
		return runRunDetail(ctx, arguments[1:], output)
	case "repos":
		return runRepos(ctx, arguments[1:], output)
	case "issues":
		return runIssues(ctx, arguments[1:], output)
	case "status":
		return runRemoteStatus(ctx, arguments[1:], output)
	case "log":
		return runRemoteLog(ctx, arguments[1:], output)
	case "uplink":
		return runUplink(ctx, arguments[1:], input, output)
	case "access":
		return runAccess(ctx, arguments[1:], output)
	case "secretstore":
		return runSecretStore(ctx, arguments[1:], output)
	case "install":
		return runInstall(ctx, arguments[1:], input, output)
	case "preload":
		return runPreload(ctx, arguments[1:], output)
	case "upgrade":
		return runUpgrade(ctx, arguments[1:], output)
	case "version":
		if len(arguments) != 1 {
			return fmt.Errorf("%w: version accepts no arguments", errUsage)
		}
		_, err := fmt.Fprintf(output, "oberth %s commit=%s date=%s\n", version, commit, date)
		return err
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, arguments[0])
	}
}
