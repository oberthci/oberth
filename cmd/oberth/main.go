package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
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

const usageCommands = "audit, init, validate, install, setup, upgrade, backup, restore, serve, upstream, repo, schedules, fragments, artifacts, runs, run, promotion, log, repos, issues, status, uplink, access, kubevirt, size, admission, secretstore, mcp, preflight, or version"

func runCLI(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	var err error
	ctx, err = withCommandProtocol(ctx)
	if err != nil {
		return err
	}
	if len(arguments) == 0 {
		return fmt.Errorf("%w: expected %s", errUsage, usageCommands)
	}
	switch arguments[0] {
	case "--help", "-h":
		_, err := fmt.Fprint(output, `Usage: oberth <command> [flags]

Commands:
  audit         Verify and inspect the audit chain
  init          Initialize a bare repository
  validate      Validate a repository's .oberth pipeline
  install       Install or upgrade Oberth on a cluster
  setup         Interactive setup wizard
  upgrade       Upgrade an existing Oberth deployment
  wait-idle     Wait for the live daemon queue to empty (in-pod)
  backup        Write an encrypted backup of OpenBao state and the server database
  restore       Restore an oberth backup onto an installed Oberth
  admin         Read local files or hash the database for maintenance
  serve         Start the Oberth server
  upstream      Manage upstream forge connections
  repo          Manage repository mappings
  schedules     List and manage scheduled runs
  fragments     Manage pipeline fragments
  artifacts     List and download build artifacts
  runs          List recent CI/CD runs, or cancel a ref's active runs
  run           Show a run; retry, rerun at the same SHA, cancel one, or purge its retained logs/artifacts
  promotion     Retry a failed or interrupted promotion onto the current target head
  log           Show step logs for a run
  repos         List registered repositories
  issues        List and manage issues
  status        Show CI status for a ref
  uplink        Manage uplink identities
  access        Manage secret access grants
  kubevirt      Manage repository VMI grants
  size          Manage repository admission size grants (default max L; XL needs a grant)
  admission     Show the server's admission reservations, or release a stale one
  secretstore   Verify and inspect the secret store
  mcp           Call Oberth MCP tools
  preflight     Release pre-flight checks
  version       Print version information
`)
		return err
	case "audit":
		return runAudit(ctx, arguments[1:], output)
	case "admin":
		return runAdmin(ctx, arguments[1:], output)
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
	case "promotion":
		return runPromotion(ctx, arguments[1:], output)
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
	case "kubevirt":
		return runKubeVirtGrant(ctx, arguments[1:], output)
	case "size":
		return runSizeGrant(ctx, arguments[1:], output)
	case "admission":
		return runAdmission(ctx, arguments[1:], output)
	case "secretstore":
		return runSecretStore(ctx, arguments[1:], output)
	case "mcp":
		return runMCP(ctx, arguments[1:], output)
	case "preflight":
		return runPreflight(ctx, arguments[1:], output)
	case "install":
		return runInstall(ctx, arguments[1:], input, output)
	case "setup":
		return runSetup(ctx, arguments[1:], input, output)
	case "wait-idle":
		return runWaitIdle(ctx, arguments[1:], output)
	case "upgrade":
		return runUpgrade(ctx, arguments[1:], output)
	case "backup":
		return runBackup(ctx, arguments[1:], input, output)
	case "restore":
		return runRestore(ctx, arguments[1:], input, output)
	case "version":
		// AI-CONTRACT: the default (no-flag) output format is consumed by
		// .oberth/release.sh verify_binary_version() which requires exactly
		// 4 space-separated fields: "oberth $tag commit=$commit date=$date".
		// Adding fields here breaks the release build. Use --schema for the
		// schema version instead.
		if len(arguments) == 1 {
			_, err := fmt.Fprintf(output, "oberth %s commit=%s date=%s\n", version, commit, date)
			return err
		}
		if len(arguments) == 2 && arguments[1] == "--schema" {
			_, err := fmt.Fprintf(output, "%d\n", store.LatestMigrationVersion())
			return err
		}
		return fmt.Errorf("%w: version accepts no arguments or --schema", errUsage)
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, arguments[0])
	}
}
