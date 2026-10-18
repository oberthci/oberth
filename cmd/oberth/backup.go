package main

// `oberth backup` and `oberth restore` (#065, #068): one file in the current
// directory, safe defaults, and flags only for the unusual case.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

// databaseSnapshotTimeout bounds the in-pod snapshot; the store's snapshot
// primitive is not itself context-interruptible.
const databaseSnapshotTimeout = 10 * time.Minute

// archivePrefix and archiveSuffix name what oberth backup writes: a
// gzip-compressed tar, encrypted (openssl cms, AES-256-GCM) because it holds
// secrets.
const (
	archivePrefix = "oberth-backup-"
	archiveSuffix = ".tar.gz.enc"
)

// commonBackupFlags are listed first in help; the rest are "Advanced".
var commonBackupFlags = map[string]bool{"output": true, "o": true, "history-only": true, "dry-run": true,
	"inspect": true, "only": true, "yes": true}

func runBackup(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) > 0 {
		switch arguments[0] {
		case "database":
			return runBackupDatabase(ctx, arguments[1:], output)
		case "keygen":
			return runBackupKeygen(arguments[1:], output)
		case "export-secrets":
			return runExportSecrets(ctx, arguments[1:], output)
		case "export-values":
			return runExportValues(ctx, arguments[1:], output)
		}
	}
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.BackupConfig
	flags.StringVar(&cfg.Output, "output", "", "write the backup to `FILE` instead of ./"+archivePrefix+"<date-time>"+archiveSuffix)
	flags.StringVar(&cfg.Output, "o", "", "shorthand for --output `FILE`")
	historyOnly := flags.Bool("history-only", false, "back up runs, issues, promotions, uplinks and step logs only: no secrets or settings, no OpenBao root token needed")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "show what would be backed up; reads no secrets and writes nothing")
	flags.StringVar(&cfg.Recipient, "recipient", "", "encrypt to this PEM `CERT` instead of ~/.config/oberth/backup/recipient.pem")
	flags.BoolVar(&cfg.NoLogs, "no-logs", false, "leave the step logs out")
	addClusterFlags(flags, &cfg.KubeconfigPath, &cfg.ContextName, &cfg.Namespace, &cfg.OpenBaoNamespace, &cfg.ArgoNamespace)
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		return printBackupUsage(flags, output)
	}
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: backup takes no arguments (write elsewhere with --output FILE)", errUsage)
	}
	if *historyOnly {
		cfg.Scope = installer.BackupScopeHistory
	}
	if cfg.Recipient == "" {
		keyPath, certPath, err := installer.BackupRecipientPaths()
		if err != nil {
			return err
		}
		_, certErr := os.Stat(certPath)
		_, keyErr := os.Stat(keyPath)
		if certErr == nil && keyErr != nil {
			// Encrypting to a certificate whose private key is gone would
			// write backups nobody can restore.
			return fmt.Errorf("%s exists but its private key %s does not: backups encrypted to it could never be restored. "+
				"Put the key back, or move %s aside so the next backup creates a new pair", displayHomePath(certPath), displayHomePath(keyPath), displayHomePath(certPath))
		}
		if certErr != nil {
			if cfg.DryRun {
				_, _ = fmt.Fprintf(output, "No backup key yet: a real backup creates %s first.\n", displayHomePath(keyPath))
			} else {
				if _, _, err := installer.GenerateBackupRecipient(filepath.Dir(keyPath)); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(output, "Created the backup key %s (first backup on this machine).\n"+
					"  Every backup is encrypted to it and cannot be restored without it: keep a copy OFF this machine.\n\n", displayHomePath(keyPath))
			}
		}
		cfg.Recipient = certPath
	}
	if cfg.Output == "" {
		cfg.Output = archivePrefix + time.Now().UTC().Format("20060102-150405") + archiveSuffix
		if abs, err := filepath.Abs(cfg.Output); err == nil {
			cfg.Output = abs
		}
	}
	cfg.ToolVersion = version
	return installer.ExecuteBackup(ctx, cfg, installer.InstallDeps{Output: output, Input: input})
}

func printBackupUsage(flags *flag.FlagSet, output io.Writer) error {
	_, _ = fmt.Fprint(output, `Usage: oberth backup [flags]

Back up this Oberth into one file in the current directory,
`+archivePrefix+`<date-time>`+archiveSuffix+`: the history (runs, issues,
promotions, uplinks, step logs), the OpenBao secrets, policies and roles, and
the settings Oberth needs. Nothing in the cluster changes.

    oberth backup                    # asks for the OpenBao root token; Enter alone = history only
    oberth backup --history-only     # no secrets, no token
    oberth restore                   # later, on a fresh install: restores the newest backup here

The file is encrypted to ~/.config/oberth/backup/recipient.key, created by
the first backup. Keep a copy of that key OFF this machine: no backup can be
restored without it.

Also:
    oberth backup export-secrets [FILE]   the secrets as plain JSON any OpenBao/Vault can import
    oberth backup export-values [FILE]    the release's Helm values as a values file
    oberth backup keygen [--dir DIR]      create the key pair ahead of time

`)
	printOperatorFlags(flags, output)
	_, _ = fmt.Fprintf(output, "\nGuide: %s\n", backupDocsURL())
	return nil
}

func runBackupKeygen(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("backup keygen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	keyPath, _, err := installer.BackupRecipientPaths()
	if err != nil {
		return err
	}
	dir := flags.String("dir", filepath.Dir(keyPath), "`DIR` for recipient.key and recipient.pem (created 0700)")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		_, _ = fmt.Fprint(output, `Usage: oberth backup keygen [--dir DIR]

Create the RSA-4096 key pair backups are encrypted to: recipient.key (the
private key, mode 0600) and recipient.pem (its certificate). oberth backup
does this itself on first use; existing files are never replaced.

`)
		printOperatorFlags(flags, output)
		return nil
	}
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: backup keygen accepts only --dir", errUsage)
	}
	key, cert, err := installer.GenerateBackupRecipient(*dir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, `Created the backup key pair:
  private key  %s (0600)
  certificate  %s

Copy the private key OFF this machine now (a password manager or another
host): every backup is encrypted to it and none can be restored without it.
`, displayHomePath(key), displayHomePath(cert))
	return nil
}

func runExportSecrets(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("backup export-secrets", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	key := flags.String("key", "", "the backup's private `KEY` instead of ~/.config/oberth/backup/recipient.key")
	out := flags.String("output", "", "write to `FILE` (created 0600) instead of standard output")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		_, _ = fmt.Fprint(output, `Usage: oberth backup export-secrets [FILE] [--output FILE.json]

Write the OpenBao KV secrets a backup holds as one plain JSON document
(format oberth.secrets/v1: KV v2 paths and their key/value data), which any
OpenBao or Vault can import with its own CLI. FILE defaults to the newest
backup in the current directory. The output is NOT encrypted: it refuses to
print to a terminal, and --output creates the file with mode 0600.

Import into a KV v2 mount with the bao (or vault) CLI:

    jq -c '.secrets[]' secrets.json | while read -r s; do
      echo "$s" | jq .data | bao kv put -mount=oberth "$(echo "$s" | jq -r .path)" -
    done

`)
		printOperatorFlags(flags, output)
		return nil
	}
	if err != nil {
		return err
	}
	// Status lines go to stderr: stdout carries the JSON document.
	archive, err := resolveArchive(positional, os.Stderr)
	if err != nil {
		return err
	}
	keyPath, err := resolveBackupKey(*key, archive)
	if err != nil {
		return err
	}
	cfg := installer.ExportSecretsConfig{Input: archive, PrivateKey: keyPath}
	if *out == "" {
		if file, ok := output.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			return fmt.Errorf("%w: export-secrets writes plaintext secrets; redirect it (> secrets.json) or pass --output FILE", errUsage)
		}
		return installer.ExportSecrets(ctx, cfg, installer.InstallDeps{Output: output}, output)
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- explicit operator output path, created exclusively.
	if err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	exportErr := installer.ExportSecrets(ctx, cfg, installer.InstallDeps{Output: output}, file)
	closeErr := file.Close()
	if err := errors.Join(exportErr, closeErr); err != nil {
		_ = os.Remove(*out)
		return err
	}
	_, _ = fmt.Fprintf(output, "Wrote %s (mode 0600, NOT encrypted: delete it once imported).\n", *out)
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

func runExportValues(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("backup export-values", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	key := flags.String("key", "", "the backup's private `KEY` instead of ~/.config/oberth/backup/recipient.key")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		_, _ = fmt.Fprint(output, `Usage: oberth backup export-values [FILE] > values.yaml

Print the Helm values of release oberth that a backup recorded, as YAML.
A restore applies only what Oberth needs (compatibility values, release WIF,
certificate names); your other settings live in your own values files, and
this recovers them when those files are gone. Values the installer sets
itself (image, secretstore, tls.pki, certificate pins) describe the source
install: keep only your own settings before oberth upgrade -f values.yaml.
FILE defaults to the newest backup in the current directory.

`)
		printOperatorFlags(flags, output)
		return nil
	}
	if err != nil {
		return err
	}
	archive, err := resolveArchive(positional, os.Stderr)
	if err != nil {
		return err
	}
	keyPath, err := resolveBackupKey(*key, archive)
	if err != nil {
		return err
	}
	return installer.ExportValues(ctx, installer.ExportSecretsConfig{Input: archive, PrivateKey: keyPath}, installer.InstallDeps{Output: output}, output)
}

func runRestore(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.RestoreConfig
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "show what would change on this install, then stop")
	flags.BoolVar(&cfg.Inspect, "inspect", false, "show what the backup holds and stop; needs no cluster")
	only := flags.String("only", "", "restore only these parts: `PARTS` from history, secrets, settings (comma-separated)")
	flags.BoolVar(&cfg.Yes, "yes", false, "skip the confirmation (for scripts)")
	flags.StringVar(&cfg.Input, "input", "", "the backup `FILE` (same as the argument)")
	flags.StringVar(&cfg.Input, "i", "", "shorthand for --input `FILE`")
	flags.StringVar(&cfg.PrivateKey, "key", "", "the backup's private `KEY` instead of ~/.config/oberth/backup/recipient.key")
	flags.StringVar(&cfg.PrivateKey, "private-key", "", "same as --key `KEY`")
	flags.BoolVar(&cfg.NoLogs, "no-logs", false, "do not restore the step logs")
	flags.BoolVar(&cfg.RestoreForgeKey, "restore-forge-key", false,
		"restore the backup's forge (upstream) SSH key even though this install has its own, which its forges accept")
	flags.StringVar(&cfg.ChartPath, "chart", "", "local Oberth `CHART` for the Helm upgrade (default: the published chart at the running version)")
	addClusterFlags(flags, &cfg.KubeconfigPath, &cfg.ContextName, &cfg.Namespace, &cfg.OpenBaoNamespace, &cfg.ArgoNamespace)
	timeout := flags.Duration("timeout", installer.DefaultTimeout, "wait timeout for pods")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		return printRestoreUsage(flags, output)
	}
	if err != nil {
		return err
	}
	if cfg.Input != "" {
		positional = append([]string{cfg.Input}, positional...)
	}
	if len(positional) > 1 {
		return fmt.Errorf("%w: restore takes one backup file or directory, got %d", errUsage, len(positional))
	}
	if cfg.Yes && len(positional) == 0 {
		return fmt.Errorf("%w: --yes restores without asking, so name the backup: oberth restore FILE --yes", errUsage)
	}
	if cfg.Input, err = resolveArchive(positional, output); err != nil {
		return err
	}
	if cfg.PrivateKey, err = resolveBackupKey(cfg.PrivateKey, cfg.Input); err != nil {
		return err
	}
	if *only != "" {
		cfg.Only = strings.Split(*only, ",")
	}
	cfg.Timeout = *timeout
	return installer.ExecuteRestore(ctx, cfg, installer.InstallDeps{Output: output, Input: input})
}

func printRestoreUsage(flags *flag.FlagSet, output io.Writer) error {
	_, _ = fmt.Fprint(output, `Usage: oberth restore [FILE | DIR] [flags]

Restore an oberth backup onto this Oberth, typically a fresh install. Without
an argument it takes the newest `+archivePrefix+`*`+archiveSuffix+` in the current
directory; a directory means the newest backup in it. It first shows what the
backup holds and exactly what would change here (history replaced, secrets
new or replaced by name, each setting from its current value), then asks you
to type the cluster's context name. This install's database is kept on its
volume in /data/pre-restore-<time>/.

    oberth restore --inspect         # what the newest backup here holds; no cluster needed
    oberth restore --dry-run         # what would change on this install; changes nothing
    oberth restore                   # restore it (asks for the OpenBao root token and a confirmation)
    oberth restore --only history    # runs, issues, step logs only; keeps this install's secrets and settings

`)
	printOperatorFlags(flags, output)
	_, _ = fmt.Fprintf(output, "\nGuide: %s\n", backupDocsURL())
	return nil
}

// addClusterFlags registers the advanced cluster-selection flags.
func addClusterFlags(flags *flag.FlagSet, kubeconfig, context, namespace, openbao, argo *string) {
	flags.StringVar(kubeconfig, "kubeconfig", "", "kubeconfig `FILE`")
	flags.StringVar(context, "context", "", "kubeconfig context `NAME`")
	flags.StringVar(namespace, "namespace", "", "Oberth `NAMESPACE` (default oberth)")
	flags.StringVar(openbao, "openbao-namespace", "", "OpenBao `NAMESPACE` (default openbao)")
	flags.StringVar(argo, "argo-namespace", "", "pipeline `NAMESPACE` (default oberth-argo)")
}

// resolveArchive picks the backup: the argument (a file, or a directory
// meaning its newest backup), else the newest backup in the current
// directory, else in ~/oberth-backups (where v0.17.31 wrote them).
func resolveArchive(positional []string, output io.Writer) (string, error) {
	if len(positional) == 1 {
		info, err := os.Stat(positional[0])
		if err != nil {
			return "", fmt.Errorf("backup: %w", err)
		}
		if !info.IsDir() {
			return positional[0], nil
		}
		newest := newestArchive(positional[0])
		if newest == "" {
			return "", fmt.Errorf("%w: no oberth backup in %s", errUsage, positional[0])
		}
		_, _ = fmt.Fprintf(output, "Using %s, the newest backup in %s.\n", filepath.Base(newest), positional[0])
		return newest, nil
	}
	for _, dir := range archiveSearchDirs() {
		if newest := newestArchive(dir); newest != "" {
			_, _ = fmt.Fprintf(output, "Using %s, the newest backup in %s (pass a file to choose another).\n", displayHomePath(newest), displayHomePath(dir))
			return newest, nil
		}
	}
	return "", fmt.Errorf("%w: no oberth backup (%s*%s) in the current directory; pass the file: oberth restore FILE", errUsage, archivePrefix, archiveSuffix)
}

func archiveSearchDirs() []string {
	dirs := []string{"."}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "oberth-backups"))
	}
	return dirs
}

// newestArchive returns the most recently modified backup in dir (and its
// subdirectories one level down), or "".
func newestArchive(dir string) string {
	var newest string
	var newestTime time.Time
	for _, pattern := range []string{"*" + archiveSuffix, "*.cms", filepath.Join("*", "*"+archiveSuffix), filepath.Join("*", "*.cms")} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, match := range matches {
			if info, err := os.Stat(match); err == nil && info.Mode().IsRegular() && info.ModTime().After(newestTime) {
				newest, newestTime = match, info.ModTime()
			}
		}
	}
	return newest
}

// resolveBackupKey picks the private key: the flag, else the default
// ~/.config/oberth/backup/recipient.key, else a recipient.key next to the
// backup (for a backup and key carried to a new machine together).
func resolveBackupKey(flagValue, archive string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	keyPath, _, err := installer.BackupRecipientPaths()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(keyPath); err == nil {
		return keyPath, nil
	}
	beside := filepath.Join(filepath.Dir(archive), "recipient.key")
	if _, err := os.Stat(beside); err == nil {
		return beside, nil
	}
	return "", fmt.Errorf("%w: no backup key: copy the recipient.key the backup was made with to %s (or next to the backup), or pass --key FILE",
		errUsage, displayHomePath(keyPath))
}

// --- Operator-friendly flag handling for backup and restore (#065) ---

// parseOperatorFlags parses flags the way a first-time operator expects:
// flags may come before or after arguments, --help or -h anywhere shows help
// (instead of becoming a flag's value), a string flag whose value is itself a
// flag is refused, and errors spell flags with two dashes as the
// documentation does. It returns the positional arguments.
func parseOperatorFlags(flags *flag.FlagSet, arguments []string) (positional []string, help bool, err error) {
	for _, arg := range arguments {
		if arg == "--" {
			break
		}
		if arg == "--help" || arg == "-help" || arg == "-h" {
			return nil, true, nil
		}
	}
	rest := arguments
	for {
		if err := flags.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("%w: %s", errUsage, doubleDashFlags(err.Error()))
		}
		rest = flags.Args()
		if len(rest) == 0 {
			break
		}
		if rest[0] == "--" {
			positional = append(positional, rest[1:]...)
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	var bad error
	flags.Visit(func(f *flag.Flag) {
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			return
		}
		if value := f.Value.String(); bad == nil && strings.HasPrefix(value, "-") {
			bad = fmt.Errorf("%w: %s needs a value, but got %q, which looks like another flag", errUsage, flagSpelling(f.Name), value)
		}
	})
	return positional, false, bad
}

var singleDashFlag = regexp.MustCompile(`(^|[\s:])-([a-zA-Z][a-zA-Z0-9-]+)`)

// doubleDashFlags rewrites the flag package's "-output" as "--output".
func doubleDashFlags(message string) string {
	return singleDashFlag.ReplaceAllString(message, "${1}--${2}")
}

func flagSpelling(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// printOperatorFlags lists the common flags, then the advanced ones.
func printOperatorFlags(flags *flag.FlagSet, output io.Writer) {
	var common, advanced []*flag.Flag
	flags.VisitAll(func(f *flag.Flag) {
		if commonBackupFlags[f.Name] {
			common = append(common, f)
		} else {
			advanced = append(advanced, f)
		}
	})
	for _, group := range []struct {
		title string
		flags []*flag.Flag
	}{{"Flags:", common}, {"Advanced:", advanced}} {
		if len(group.flags) == 0 {
			continue
		}
		_, _ = fmt.Fprintln(output, group.title)
		sort.Slice(group.flags, func(i, j int) bool { return group.flags[i].Name < group.flags[j].Name })
		for _, f := range group.flags {
			kind, usage := flag.UnquoteUsage(f)
			name := flagSpelling(f.Name)
			if kind != "" {
				name += " " + kind
			}
			_, _ = fmt.Fprintf(output, "  %-28s %s\n", name, usage)
		}
	}
}

// backupDocsURL is the backup guide for this build: installed hosts carry no
// docs/ directory, so a repository path is no help there (#065).
func backupDocsURL() string {
	ref := "main"
	if strings.HasPrefix(version, "v") && !strings.ContainsAny(version, "-+") {
		ref = version
	}
	return "https://github.com/oberthci/oberth/blob/" + ref + "/docs/backup-restore.md"
}

// displayHomePath shows a path under the home directory as ~/...
func displayHomePath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
			return "~/" + filepath.ToSlash(rest)
		}
	}
	return path
}
