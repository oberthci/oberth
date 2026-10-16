package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

// databaseSnapshotTimeout bounds the in-pod snapshot; the store's snapshot
// primitive is not itself context-interruptible.
const databaseSnapshotTimeout = 10 * time.Minute

func runBackup(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "database" {
		return runBackupDatabase(ctx, arguments[1:], output)
	}
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.BackupConfig
	flags.StringVar(&cfg.Output, "output", "", "encrypted archive to create (must not exist)")
	flags.StringVar(&cfg.Output, "o", "", "shorthand for --output")
	flags.StringVar(&cfg.Recipient, "recipient", "", "PEM certificate (RSA >= 3072 bits) the archive is encrypted to")
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig file")
	flags.StringVar(&cfg.ContextName, "context", "", "context in the selected kubeconfig")
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", "", "OpenBao namespace (default: openbao)")
	flags.StringVar(&cfg.ArgoNamespace, "argo-namespace", "", "pipeline namespace (default: oberth-argo)")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "print what would be backed up without reading secrets or writing anything")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return printBackupUsage(flags, output)
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: backup accepts flags only, no positional arguments", errUsage)
	}
	cfg.ToolVersion = version
	return installer.ExecuteBackup(ctx, cfg, installer.InstallDeps{Output: output, Input: input})
}

func printBackupUsage(flags *flag.FlagSet, output io.Writer) error {
	_, _ = fmt.Fprint(output, `Usage: oberth backup --output FILE --recipient CERT.pem [flags]

Write one encrypted archive of this installation's state: OpenBao KV secrets,
policies and Kubernetes auth roles, a verified server database snapshot with
its compatibility values, the release's Helm values and the secret-access
ConfigMap. Requires kubectl, helm, openssl and BAO_TOKEN (read it with
'read -rs BAO_TOKEN && export BAO_TOKEN'). See docs/backup-restore.md.

`)
	flags.SetOutput(output)
	flags.PrintDefaults()
	return nil
}

// runBackupDatabase is the in-pod half of `oberth backup`: it writes the
// store's verified database archive (oberth.database-backup/v1) to stdout,
// under the deployment's own protocol identifiers. Run through kubectl exec
// in the server container; it never opens the live database writable.
func runBackupDatabase(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("backup database", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 || strings.TrimSpace(*database) == "" {
		return fmt.Errorf("%w: backup database accepts only --database", errUsage)
	}
	if file, ok := output.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return errors.New("backup database writes a binary archive; redirect stdout (oberth backup runs it through kubectl exec)")
	}
	ctx, cancel := context.WithTimeout(ctx, databaseSnapshotTimeout)
	defer cancel()
	if _, err := store.WriteVerifiedBackup(ctx, *database, store.LatestMigrationVersion(), configuredStoreOptions(ctx), output); err != nil {
		return fmt.Errorf("database snapshot: %w", err)
	}
	return nil
}

func runRestore(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.RestoreConfig
	flags.StringVar(&cfg.Input, "input", "", "archive written by oberth backup")
	flags.StringVar(&cfg.Input, "i", "", "shorthand for --input")
	flags.StringVar(&cfg.PrivateKey, "private-key", "", "PEM private key of the archive's recipient (mode 0600)")
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig file")
	flags.StringVar(&cfg.ContextName, "context", "", "context in the selected kubeconfig")
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", "", "OpenBao namespace (default: openbao)")
	flags.StringVar(&cfg.ArgoNamespace, "argo-namespace", "", "pipeline namespace (default: oberth-argo)")
	flags.StringVar(&cfg.ChartPath, "chart", "",
		"local Oberth chart to use when compatibility values must be set on the release "+
			"(default: the published chart at the release's current version)")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "decrypt, verify and print the plan without changing anything")
	flags.BoolVar(&cfg.Yes, "yes", false, "apply the plan without the interactive confirmation")
	timeout := flags.Duration("timeout", installer.DefaultTimeout, "wait timeout for pods")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return printRestoreUsage(flags, output)
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: restore accepts flags only, no positional arguments", errUsage)
	}
	cfg.Timeout = *timeout
	return installer.ExecuteRestore(ctx, cfg, installer.InstallDeps{Output: output, Input: input})
}

func printRestoreUsage(flags *flag.FlagSet, output io.Writer) error {
	_, _ = fmt.Fprint(output, `Usage: oberth restore --input FILE --private-key KEY.pem [--dry-run | --yes] [flags]

Restore an oberth backup archive onto an installed Oberth (typically fresh):
verify the archive and database offline, show the plan, then write OpenBao
state, swap the server database (the previous one is kept in
/data/pre-restore-<timestamp>/), restore the secret-access ConfigMap, set the
compatibility values and remove orphaned workflows. Requires kubectl, helm,
openssl and BAO_TOKEN. See docs/backup-restore.md.

`)
	flags.SetOutput(output)
	flags.PrintDefaults()
	return nil
}
