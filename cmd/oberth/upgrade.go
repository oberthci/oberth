package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/store"
)

func runUpgrade(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.UpgradeConfig
	flags.BoolVar(&cfg.ArgoSecurityDefaults, "argo-security-defaults", false, "consent to filling absent native security defaults on an existing Argo release; preserve operator values")
	flags.BoolVar(&cfg.ReissueServerCA, "reissue-server-ca", false, "allow server CA replacement after reviewing its planned names; clients must refresh the CA pin")
	var kubeContext string
	var userValues, userSet stringList
	var tlsDNS, tlsIPs, removeDNS, removeIPs stringList
	flags.Var(&tlsDNS, "tls-extra-dns-name", "add a server TLS DNS name (repeatable)")
	flags.Var(&tlsIPs, "tls-extra-ip", "add a server TLS IP SAN (repeatable)")
	flags.Var(&removeDNS, "tls-remove-dns-name", "remove a permitted DNS name; refused while served (repeatable)")
	flags.Var(&removeIPs, "tls-remove-ip", "remove a permitted IP SAN; refused while served (repeatable)")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", "", "OpenBao namespace (default: openbao)")
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&kubeContext, "context", "", "kubeconfig context to upgrade (required when the kubeconfig has more than one context)")
	flags.Bool("wait-idle", true, "wait for active Workflows before a version change (the default)")
	flags.BoolVar(&cfg.NoWaitIdle, "no-wait-idle", false, "skip waiting; running Workflows of the previous version will fail on their next step")
	flags.DurationVar(&cfg.WaitTimeout, "wait-for", installer.DefaultUpgradeWaitTimeout, "maximum wait for active Workflows before a version change, independent of --timeout")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "show what would change without doing it")
	flags.BoolVar(&cfg.Yes, "yes", false, "with --rollback: accept the plan, including replacing the server database with the pre-upgrade backup")
	flags.StringVar(&cfg.ChartOverride, "chart", "", "deploy this chart instead of the signed release (development only: not verified)")
	addReleaseSourceFlags(flags, &cfg.ReleaseDir, &cfg.ReleaseOrigin, &cfg.ChartRepository)
	flags.Var(&userValues, "values", "values file to pass to helm upgrade (repeatable)")
	flags.Var(&userValues, "f", "alias for --values")
	flags.Var(&userSet, "set", "one canonical dotted key=scalar per flag; escape literal commas (repeatable)")
	flags.BoolVar(&cfg.NoBackup, "no-backup", false, "upgrade without the pre-upgrade history backup (a rollback across a schema migration is then impossible)")
	backupRecipient := flags.String("backup-recipient", "", "encrypt the pre-upgrade backup to this PEM `CERT` you manage instead of ~/.config/oberth/backup/recipient.pem: no backup key is created or confirmed here, so it needs no terminal")
	backupDir := flags.String("backup-dir", "", "write the pre-upgrade backup and the rollback record to `DIR` (default: the current directory)")
	flags.BoolVar(&cfg.AllowDowngrade, "allow-downgrade", false, "accept a CLI older than the installed release (a deliberate rollback within one database schema)")
	forceRecord := flags.Bool("force-record", false, "DANGEROUS, with --rollback: accept a stale record although either release changed after its upgrade; later changes are undone and the database may be replaced by an old backup (check helm history first)")
	rollback := flags.String("rollback", "", "roll back the upgrade recorded in `FILE` (the rollback record an upgrade printed): Helm revisions, then the pre-upgrade backup when a migration ran")
	timeout := flags.Duration("timeout", 5*time.Minute, "wait timeout for rollout completion")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: upgrade accepts flags only, no positional arguments", errUsage)
	}
	if *rollback != "" {
		return runUpgradeRollback(ctx, *rollback, kubeContext, cfg, *forceRecord, *timeout, input, output)
	}
	if cfg.Yes {
		return fmt.Errorf("%w: --yes applies to --rollback only; an upgrade needs no confirmation flag", errUsage)
	}
	if cfg.NoBackup && *backupRecipient != "" {
		return fmt.Errorf("%w: --backup-recipient and --no-backup contradict each other", errUsage)
	}

	if cfg.WaitTimeout <= 0 {
		return fmt.Errorf("%w: --wait-for must be positive", errUsage)
	}
	cfg.Timeout = *timeout
	cfg.BinaryVersion = version
	cfg.UserValues = []string(userValues)
	cfg.UserSet = []string(userSet)
	cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = tlsDNS, tlsIPs
	cfg.TLSRemoveDNSNames, cfg.TLSRemoveIPs = removeDNS, removeIPs
	cfg.BinarySchemaVersion = store.LatestMigrationVersion()

	// Validate early so dev-build rejection does not require a cluster.
	if err := cfg.ValidateUpgrade(); err != nil {
		return err
	}

	recordDir := *backupDir
	if recordDir == "" {
		recordDir = "."
	}
	absDir, err := filepath.Abs(recordDir)
	if err != nil {
		return err
	}
	cfg.RecordDir = absDir
	if info, statErr := os.Stat(cfg.RecordDir); statErr != nil || !info.IsDir() {
		return fmt.Errorf("%w: --backup-dir %s is not a directory", errUsage, recordDir)
	}
	if !cfg.NoBackup {
		cfg.Backup = func(ctx context.Context) (installer.UpgradeBackup, error) {
			return takePreUpgradeBackup(ctx, cfg.RecordDir, *backupRecipient, kubeContext, cfg.Namespace, input, output)
		}
	}

	if err := installer.RequireContextChoice("upgrade", kubeContext, "", kubeconfigContexts); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	kubeClient, restConfig, selectedContext, err := installer.LoadKubeConfigForContext(kubeContext, output)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Resolve helm kubeconfig targeting so helm upgrade targets the same
	// cluster the binary resolved (#464).
	helmKubeArgs, _, _ := installer.ResolveHelmKubeArgs(kubeContext)
	runHelm := installer.HelmWithKubeArgs(installer.DefaultRunHelm, helmKubeArgs)

	result, err := installer.RunUpgrade(ctx, cfg, installer.Deps{
		Output:           output,
		Input:            input,
		IsTerminal:       installer.StdinIsTerminal,
		RunHelm:          runHelm,
		RunCommand:       installer.DefaultRunCommand,
		RunCommandStream: installer.DefaultRunCommandStream,
		KubeClient:       kubeClient,
		RestConfig:       restConfig,
		ContextName:      selectedContext,
	})
	if err != nil {
		return err
	}
	return upgradeCompletionError(cfg.DryRun, result)
}

// kubeconfigContexts lists the contexts an upgrade could reach; tests replace
// it.
var kubeconfigContexts = installer.KubeconfigContexts

func upgradeCompletionError(dryRun bool, result installer.UpgradeResult) error {
	if dryRun || result.Upgraded || result.AlreadyUpToDate {
		return nil
	}
	return errors.New("upgrade not performed")
}

// takePreUpgradeBackup is oberth backup --history-only, run by oberth upgrade
// before its first change against the same context and namespace.
func takePreUpgradeBackup(ctx context.Context, dir, recipient, kubeContext, namespace string, input io.Reader, output io.Writer) (installer.UpgradeBackup, error) {
	if recipient == "" {
		var err error
		if recipient, err = defaultBackupRecipient(false, input, output); err != nil {
			return installer.UpgradeBackup{}, fmt.Errorf("%w; for this upgrade, --backup-recipient CERT encrypts the backup to a certificate you manage", err)
		}
	} else if _, err := os.Stat(recipient); err != nil {
		return installer.UpgradeBackup{}, fmt.Errorf("--backup-recipient: %w", err)
	}
	archive := filepath.Join(dir, archivePrefix+time.Now().UTC().Format("20060102-150405")+archiveSuffix)
	cfg := installer.BackupConfig{
		MaxDatabaseBytes: store.DefaultBackupMaxBytes,
		ContextName:      kubeContext,
		Namespace:        namespace,
		Output:           archive,
		Recipient:        recipient,
		Scope:            installer.BackupScopeHistory,
		ToolVersion:      version,
	}
	if err := installer.ExecuteBackup(ctx, cfg, installer.InstallDeps{Output: output, Input: input}); err != nil {
		return installer.UpgradeBackup{}, err
	}
	return installer.UpgradeBackup{Path: archive}, nil
}

// addReleaseSourceFlags registers where a signed release is read from; the
// verification against the compiled-in key is the same for every source.
func addReleaseSourceFlags(flags *flag.FlagSet, dir, origin, repository *string) {
	flags.StringVar(dir, "release-dir", "", "read the signed release (release.json, oberth-<version>.tgz and their .sigstore.json bundles) from `DIR` instead of fetching it")
	flags.StringVar(origin, "release-origin", "", "fetch release.json from this HTTPS origin instead of "+releaseverify.DefaultReleaseOrigin)
	flags.StringVar(repository, "chart-repository", "", "fetch the chart from this HTTPS origin instead of "+releaseverify.DefaultChartRepository)
}

// runUpgradeRollback rolls back one recorded upgrade; a restore of the
// pre-upgrade backup runs through oberth restore --only history.
func runUpgradeRollback(ctx context.Context, record, kubeContext string, cfg installer.UpgradeConfig, forceRecord bool, timeout time.Duration, input io.Reader, output io.Writer) error {
	if err := installer.RequireContextChoice("roll back", kubeContext, "", kubeconfigContexts); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	kubeClient, restConfig, selectedContext, err := installer.LoadKubeConfigForContext(kubeContext, output)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	helmKubeArgs, _, _ := installer.ResolveHelmKubeArgs(kubeContext)
	// The restore targets the record's installation (its namespace and that
	// release's pipeline namespace), never this command's defaults.
	restore := func(ctx context.Context, target installer.RollbackRestore) error {
		key, err := resolveBackupKey("", target.Archive)
		if err != nil {
			return err
		}
		return installer.ExecuteRestore(ctx, installer.RestoreConfig{
			MaxDatabaseBytes: store.DefaultBackupMaxBytes,
			ContextName:      kubeContext,
			Namespace:        target.Namespace,
			ArgoNamespace:    target.ArgoNamespace,
			Input:            target.Archive,
			PrivateKey:       key,
			Only:             []string{installer.RestorePartHistory},
			InterruptQueued:  true,
			Release:          installer.ReleaseSource{Dir: cfg.ReleaseDir, Origin: cfg.ReleaseOrigin, ChartRepository: cfg.ChartRepository},
			Yes:              true,
			Timeout:          timeout,
		}, installer.InstallDeps{Output: output, Input: input})
	}
	return installer.RunRollback(ctx, installer.RollbackConfig{
		Record: record, Namespace: cfg.Namespace, DryRun: cfg.DryRun, Yes: cfg.Yes, ForceRecord: forceRecord, Timeout: timeout, Restore: restore,
	}, installer.Deps{
		Output:      output,
		RunHelm:     installer.HelmWithKubeArgs(installer.DefaultRunHelm, helmKubeArgs),
		RunCommand:  installer.DefaultRunCommand,
		KubeClient:  kubeClient,
		RestConfig:  restConfig,
		ContextName: selectedContext,
	})
}
