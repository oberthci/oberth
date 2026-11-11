package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func runPreflightPKI(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("preflight pki", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg installer.Config
	var kubeContext string
	var dns, ips stringList
	flags.StringVar(&cfg.Namespace, "namespace", installer.DefaultNamespace, "Oberth namespace")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", installer.DefaultOpenBaoNamespace, "OpenBao namespace")
	flags.StringVar(&kubeContext, "context", "", "kubeconfig context")
	flags.Var(&dns, "tls-extra-dns-name", "expected DNS name (repeatable)")
	flags.Var(&ips, "tls-extra-ip", "expected IP SAN (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: preflight pki accepts flags only", errUsage)
	}
	client, _, selectedContext, err := installer.LoadKubeConfigForContext(kubeContext, output)
	if err != nil {
		return err
	}
	kubeArgs, _, err := installer.ResolveHelmKubeArgs(kubeContext)
	if err != nil {
		return err
	}
	cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = dns, ips
	if err := installer.PreflightPKI(ctx, cfg, installer.Deps{Output: output, KubeClient: client,
		ContextName: selectedContext, RunCommand: installer.DefaultRunCommand,
		RunHelm: installer.HelmWithKubeArgs(installer.DefaultRunHelm, kubeArgs)}); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(output, "preflight pki: OK — the role permits the expected and currently served SANs")
	return nil
}
