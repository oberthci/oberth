package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func runUnseal(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("unseal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.UnsealConfig
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "Kubernetes config file (default: KUBECONFIG or ~/.kube/config)")
	flags.StringVar(&cfg.ContextName, "context", "", "selected kubeconfig context")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", installer.DefaultOpenBaoNamespace, "OpenBao namespace")
	flags.StringVar(&cfg.SecretsFile, "secrets-file", "", "owner-only installer recovery file; never printed")
	flags.StringVar(&cfg.AgeIdentity, "age-identity", "", "owner-only age identity file for encrypted recovery")
	flags.StringVar(&cfg.SealToHost, "seal-to-host", "", "write a TPM2-bound systemd credential to a new file on this single Linux node; anyone booting its disk here gets the secrets")
	flags.BoolVar(&cfg.Yes, "yes", false, "accept the host-bound auto-unseal risk shown by --seal-to-host")
	flags.BoolVar(&cfg.StatusOnly, "status", false, "read seal status only; do not read recovery material")
	jsonOutput := flags.Bool("json", false, "print only initialized/sealed status as JSON")
	positional, help, err := parseOperatorFlags(flags, args)
	if help {
		_, _ = fmt.Fprintln(output, "Usage: oberth unseal --secrets-file FILE [--age-identity FILE] [--context NAME]\nRead the saved key privately and unseal OpenBao. --status reads only seal status.")
		flags.SetOutput(output)
		flags.PrintDefaults()
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: invalid unseal arguments; see oberth unseal --help", errUsage)
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: unseal takes no positional arguments", errUsage)
	}
	if cfg.StatusOnly && (cfg.SecretsFile != "" || cfg.AgeIdentity != "" || cfg.SealToHost != "") {
		return usageError(errors.New("--status cannot be combined with recovery or TPM2 options"))
	}
	if !cfg.StatusOnly && cfg.SecretsFile == "" {
		return usageError(errors.New("--secrets-file is required; never paste an unseal key into the command"))
	}
	if cfg.SealToHost != "" && !cfg.Yes {
		return usageError(errors.New("anyone who boots this disk on this machine gets the secrets with auto-unseal; pass --yes only after accepting that risk"))
	}
	status, err := installer.ExecuteUnseal(ctx, cfg, installer.InstallDeps{Output: output})
	if err != nil {
		return err
	}
	return installer.PrintSealStatus(output, status, *jsonOutput)
}
