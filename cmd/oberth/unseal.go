package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func runUnseal(ctx context.Context, arguments []string, output io.Writer) error {
	var cfg installer.UnsealConfig
	var credential string
	var acknowledge bool
	flags := flag.NewFlagSet("unseal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.SecretsFile, "secrets-file", "", "owner-only installer secrets file; contents are never displayed")
	flags.StringVar(&cfg.AgeIdentity, "age-identity", "", "age identity file used to decrypt an encrypted secrets file")
	flags.StringVar(&cfg.Context, "context", "", "Kubernetes context chosen by the user (required)")
	flags.StringVar(&cfg.Namespace, "namespace", "openbao", "OpenBao namespace")
	flags.StringVar(&cfg.Pod, "pod", "openbao-0", "OpenBao pod")
	flags.StringVar(&credential, "seal-to-tpm2", "", "create a new TPM2-bound systemd credential file instead of unsealing")
	flags.BoolVar(&acknowledge, "acknowledge-host-risk", false, "accept that anyone who boots this disk on this machine gets the secrets")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return fmt.Errorf("%w: invalid unseal arguments; see oberth unseal --help", errUsage)
	}
	if flags.NArg() != 0 || cfg.SecretsFile == "" || cfg.Context == "" {
		return fmt.Errorf("%w: unseal needs --secrets-file and an explicit --context", errUsage)
	}
	if credential != "" {
		if !acknowledge {
			return errors.New("auto-unseal is off by default: anyone who boots the disk on this machine gets the secrets; explicit --acknowledge-host-risk is required")
		}
		if err := installer.SealUnsealCredential(ctx, cfg, credential, nil); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "TPM2-bound auto-unseal credential created")
		return err
	}
	if acknowledge {
		return fmt.Errorf("%w: --acknowledge-host-risk requires --seal-to-tpm2", errUsage)
	}
	sealed, err := installer.Unseal(ctx, cfg, nil)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "sealed: %t\n", sealed)
	return err
}
