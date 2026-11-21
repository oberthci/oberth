package main

import (
	"flag"
	"io"
	"math"
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
		{"backup", "Encrypted database copy for safekeeping; archive restore is not in this release.", 0, 0},
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
		f.Bool("no-link-local-filter", false, "Disable the Link-local egress filter.")
		f.String("external-store", "", "External secret-store URL.")
	case "upgrade":
		f.String("version", "", "Target release version.")
		f.Bool("no-backup", false, "Explicitly omit the upgrade backup.")
	case "secret put":
		f.String("repo", "", "Repository receiving the secret.")
		f.String("file", "", "Authored pipeline file.")
		f.String("step", "", "Authorized logical step.")
	case "backup":
		f.String("out", "", "New backup destination.")
		f.Bool("dry-run", false, "Print the planned destination without creating a backup.")
	}
	addInstallerInputs(f, name)
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

func addInstallerInputs(f *flag.FlagSet, name string) {
	if name == "rollback" {
		f.String("release-dir", "", "Signed source-release files, or version subdirectories.")
	}
	switch name {
	case "install", "upgrade":
		if name == "install" {
			f.String("chart", "", "Unverified local chart for development.")
		}
		f.String("release-dir", "", "Signed release files, or version subdirectories.")
		f.Bool("detail", false, "Show every reviewed public value.")
		f.String("output", "text", "Review format: text or json; json requires --dry-run.")
		if name != "upgrade" {
			f.String("version", "", "Exact signed release version.")
		}
	}
	switch name {
	case "install", "upgrade", "rollback", "unseal", "backup":
		f.String("profile", "", "Installation profile; defaults to the selected profile.")
	}
	switch name {
	case "backup":
		f.String("config", "", "Installation configuration for its explicit database protocol.")
	}
	switch name {
	case "secretstore setup", "secretstore check":
		f.String("config", "", "Strict server configuration with the store audience.")
		f.String("ca-file", "", "Secret-store TLS CA certificate.")
		f.String("token-file", "", "Private store token, or the step's projected login token.")
	}
	if name == "backup" {
		f.String("output", "text", "Result format: text or json.")
	}
}
