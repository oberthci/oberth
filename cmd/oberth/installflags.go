package main

import (
	"errors"
	"flag"
	"io"
	"math"
	"strings"
)

type installationCommand struct {
	name, summary    string
	minArgs, maxArgs int
}

func installationCommands() []installationCommand {
	return []installationCommand{
		{"install", "Review or install Oberth in the chosen context.", 0, 0},
		{"upgrade", "Upgrade Oberth using a signed release.", 0, 0},
		{"rollback", "Roll back the installation to its prior release.", 0, 0},
		{"backup", "Back up database, store data and installation Secrets.", 0, 0},
		{"backup verify", "Verify backup origin and decrypt contents offline.", 0, 0},
		{"restore", "Review or restore an encrypted installation backup.", 0, 0},
		{"unseal", "Unseal the installation's secret store.", 0, 0},
		{"secret put", "Store a secret value from standard input.", 1, 1},
		{"secretstore exec", "Run a step command with its admitted secrets.", 1, math.MaxInt},
		{"secretstore setup", "Configure static secret-store rules.", 0, 0},
		{"secretstore check", "Check secret-store readiness and configuration.", 0, 0},
	}
}

// Syntax belongs to the CLI; the installed owner engine decodes Choices and
// owns review, confirmation, custody and all effects.
func installationFlags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Bool("detail", false, "Show complete public diagnostics.")
	switch name {
	case "install":
		f.String("context", "", "Chosen Kubernetes context.")
		f.String("namespace", "", "Server namespace (default oberth).")
		f.String("pipeline-namespace", "", "Pipeline namespace (default <namespace>-runs).")
		f.Int("https-node-port", 0, "HTTPS Service node port (default 30443).")
		f.Int("ssh-node-port", 0, "SSH Service node port (default 30022).")
		f.String("url", "", "Public installation URL.")
		f.Int("ssh-port", 0, "Public SSH port.")
		f.Bool("verify", false, "Verify the existing installation.")
		f.Int("parallel", 0, "Maximum concurrent runs.")
		f.Bool("no-pod-defaults", false, "Disable Baseline Pod defaults.")
		f.String("external-store", "", "External secret-store URL.")
	case "upgrade":
		f.String("version", "", "Target release version.")
		f.Bool("no-backup", false, "Explicitly omit the upgrade backup.")
	case "secret put":
		f.String("repo", "", "Repository receiving the secret.")
		f.String("file", "", "Authored pipeline file.")
		f.String("step", "", "Authorized logical step.")
	case "secretstore setup":
		f.Bool("backup-only", false, "Provision snapshot access with delegated or saved unseal authority.")
		f.String("unseal-file", "", "Saved bundled-store share for one-time backup setup without a retained root session.")

	}
	addArchiveFlags(f, name)
	addInstallerInputs(f, name)
	switch name {
	case "install", "upgrade":
		f.String("resources", "", "Native resources by server, controller, store or executor as JSON; drained rollout required.")
		f.String("run-policy", "", "Opt-in namespace defaults and aggregate quotas as JSON; requires G2 migration receipts.")
		f.Int("service-port", 0, "In-cluster HTTPS Service port (default 8443).")
		f.Bool("no-egress", false, "Disable the run Pod egress policy entirely.")
		f.Bool("egress-internet", true, "Allow public internet egress; false keeps only explicit destinations and required cluster services.")
		f.Var(&egressDestinations{}, "egress-allow", "Extra CIDR, port and TCP/UDP protocol entries as JSON; [] clears the list.")
		f.Var(&clusterCIDRs{}, "cluster-cidrs", "Cluster Pod and Service CIDRs as JSON, in addition to discovered ranges.")
		f.Var(&sourcePinFlag{}, "source-pin", "Allow CONSUMER=PRODUCER source pins; repeatable, kept by later upgrades.")
		f.Bool("no-source-pins", false, "Drop the saved source pins before adding --source-pin pairs.")
	}
	switch name {
	case "install", "upgrade", "rollback":
		f.Bool("dry-run", false, "Review without applying.")
	}
	switch name {
	case "install", "upgrade", "rollback":
		f.Bool("yes", false, "Apply the reviewed operation.")
	}
	return f
}

func addArchiveFlags(f *flag.FlagSet, name string) {
	switch name {
	case "backup":
		f.Bool("allow-shared-directory", false, "Allow other installations' backups; prune only this installation's signed full archives.")
		f.String("out", "", "New backup destination.")
		f.Bool("print-schedule", false, "Print an operator-applied CronJob and scoped backup permissions.")
		f.Bool("in-cluster", false, "Use only the Pod's scoped backup ServiceAccount.")
		f.String("namespace", "", "Server namespace of the backup ServiceAccount.")
		f.String("directory", "", "Backup directory; retain only the newest named backups.")
		f.Int("keep", 7, "Number of directory backups to keep, from 1 to 1000.")
		f.Bool("dry-run", false, "Print the planned destination without creating a backup.")
		f.Bool("database-only", false, "Explicitly exclude store data and installation Secrets.")
	case "restore", "backup verify":
		f.String("from", "", "Encrypted installation backup to inspect.")
		f.String("identity-file", "", "Private age identity file; defaults to the selected profile.")
		f.String("sha256", "", "Independently saved archive SHA-256; alternative to a trusted signing profile.")
		f.String("backup-profile", "", "Original operator profile holding the trusted backup public key.")
	}
	if name == "restore" {
		f.Bool("dry-run", false, "Review contents and integrity offline.")
		f.Bool("yes", false, "Restore into a fresh cluster and profile.")
		f.String("context", "", "Explicit fresh-cluster Kubernetes context.")
		f.String("url", "", "Public URL on the fresh cluster.")
		f.String("admin-key-file", "", "Separate saved administrator private file.")
		f.String("unseal-file", "", "Separate saved store unseal file.")
	}
}

// sourcePinFlag collects repeated CONSUMER=PRODUCER pairs in order; the
// installer validates both names.
type sourcePinFlag struct{ pairs []string }

func (f *sourcePinFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.pairs, ",")
}

func (f *sourcePinFlag) Set(value string) error {
	consumer, producer, ok := strings.Cut(value, "=")
	if !ok || consumer == "" || producer == "" {
		return errors.New("use --source-pin CONSUMER=PRODUCER")
	}
	f.pairs = append(f.pairs, value)
	return nil
}

func addInstallerInputs(f *flag.FlagSet, name string) {
	if name == "rollback" {
		f.String("release-dir", "", "Signed source-release files, or version subdirectories.")
	}
	switch name {
	case "install", "upgrade", "restore":
		if name == "install" {
			f.String("chart", "", "Unverified local chart for development.")
		}
		f.String("release-dir", "", "Signed release files, or version subdirectories.")
		if name != "restore" {
			f.String("output", "text", "Review format: text or json; json requires --dry-run.")
		}
		if name != "upgrade" {
			f.String("version", "", "Exact signed release version.")
		}
	}
	switch name {
	case "install", "upgrade", "rollback", "unseal", "backup", "backup verify", "restore":
		f.String("profile", "", "Installation profile; defaults to the selected profile.")
	}
	switch name {
	case "backup", "backup verify", "restore":
		f.String("config", "", "Installation configuration for its explicit database protocol.")
	}
	switch name {
	case "secretstore setup", "secretstore check":
		f.String("config", "", "Strict server configuration with the store audience.")
		f.String("ca-file", "", "Secret-store TLS CA certificate.")
		f.String("token-file", "", "Private store token, or the step's projected login token.")
	}
	if name == "backup" || name == "backup verify" || name == "restore" {
		f.String("output", "text", "Result format: text or json.")
	}
}
