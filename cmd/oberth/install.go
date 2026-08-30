package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func runInstall(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.Config
	flags.BoolVar(&cfg.Dev, "dev", false, "install in dev/evaluation mode (default)")
	flags.BoolVar(&cfg.Production, "production", false, "install in production mode (not implemented)")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "print plan without making changes")
	flags.BoolVar(&cfg.Yes, "yes", false, "proceed even for non-local clusters")
	flags.BoolVar(&cfg.Upgrade, "upgrade", false,
		"force helm upgrade of existing releases even when already up to date "+
			"(re-runs auto-upgrade when a newer chart is targeted; this flag forces reconciliation)")
	flags.BoolVar(&cfg.InstallSecretStore, "install-secretstore", false,
		"also install a production-mode OpenBao (persistent storage; verified TLS bootstrapped directly on its PVC; initialized and unsealed via kubectl exec, "+
			"root token and unseal key printed once) and configure it as Oberth's release secret store")
	flags.BoolVar(&cfg.InstallSecretStoreDev, "install-secretstore-dev", false,
		"also install a dev-mode OpenBao (in-memory, auto-unsealed, well-known root token) and configure it "+
			"as Oberth's release secret store — evaluation only")
	flags.BoolVar(&cfg.InstallRekor, "install-rekor", false,
		"also install a local Rekor transparency-log stack (MySQL + Trillian + Rekor) for the audit witness")
	flags.StringVar(&cfg.AcceptWitnessGenesis, "accept-witness-genesis", "",
		"one-shot acknowledgment of the existing deployment's exact audit chain head "+
			"(<auditID>:<sha256hex>, from 'oberth audit head') when enabling --install-rekor "+
			"on a deployment that already has audit history")
	flags.BoolVar(&cfg.SkipArgo, "skip-argo-workflows", false,
		"do not install the Argo Workflows controller, because this cluster already runs one watching "+
			"--argo-namespace (the install still binds Oberth to it and still verifies it is ready)")
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", "", "OpenBao namespace (default: openbao)")
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&cfg.RekorNamespace, "rekor-namespace", "", "Rekor namespace (default: rekor)")
	flags.StringVar(&cfg.ArgoNamespace, "argo-namespace", "",
		"namespace pipelines execute in; must differ from --namespace (default: oberth-argo)")
	flags.StringVar(&cfg.ChartVersion, "chart-version", "", "Oberth chart version (default: binary version)")
	flags.StringVar(&cfg.ShellProfile, "shell-profile", "",
		"append the environment sourcing line to your shell profile without asking: yes or no "+
			"(default: ask when there is a terminal, and do nothing when there is not)")
	flags.StringVar(&cfg.ChartPath, "chart", "",
		"install the Oberth chart from this local directory or archive instead of the published repository")
	flags.StringVar(&cfg.ImageRef, "image", "",
		"server image the chart deploys, overriding the chart default (used with --chart for local iteration)")
	flags.StringVar(&cfg.OpenBaoChartVersion, "openbao-chart-version", "", "OpenBao chart version")
	flags.StringVar(&cfg.RekorChartVersion, "rekor-chart-version", "", "Rekor chart version")
	flags.StringVar(&cfg.ArgoChartVersion, "argo-chart-version", "", "argo-workflows chart version")
	flags.StringVar(&cfg.ArgoVaultAddress, "argo-vault-address", "",
		"OpenBao/Vault base URL injected into credentialed pipeline containers as VAULT_ADDR (https only)")
	flags.StringVar(&cfg.NetworkPolicy, "network-policy", "",
		"pipeline egress NetworkPolicy: auto (default: enable on conforming CNIs, disable on k3s+kube-router), "+
			"true (always enable), false (always disable)")
	flags.StringVar(&cfg.ArgoVaultCredentialedRole, "argo-vault-credentialed-role", "",
		"Kubernetes-auth role credentialed pipelines log in with; bind it to exactly "+
			"(--argo-namespace, the credentialed ServiceAccount)")
	var credentialedSecretPaths stringList
	flags.Var(&credentialedSecretPaths, "credentialed-secret-path",
		"full KV data path approved in the secret-access table (repeatable, exactly the string "+
			"`oberth access allow` records, e.g. oberth/data/release/cosign-secret); synced into the "+
			"RELEASE-tier credentialed Vault policy as an exact read grant when --install-secretstore "+
			"reconciles the store — the branch-tier ci-secrets policy never receives grants")
	var tlsExtraDNSNames stringList
	flags.Var(&tlsExtraDNSNames, "tls-extra-dns-name",
		"additional DNS name the generated server certificate is issued for (repeatable), beyond the "+
			"in-cluster names; a client reaching the server on any other name hits a hostname mismatch "+
			"that no trust anchor can fix. A macOS kind install adds localhost automatically")
	var tlsExtraIPs stringList
	flags.Var(&tlsExtraIPs, "tls-extra-ip",
		"additional IP address the generated server certificate is issued for (repeatable); an address "+
			"reached as an IP needs an IP SAN, because a DNS name never matches one. A macOS kind "+
			"install adds 127.0.0.1 automatically")
	var valuesFiles stringList
	flags.Var(&valuesFiles, "values",
		"helm values file applied to the Oberth release (repeatable, applied in order); "+
			"the installer's own settings still win, so a value it computed cannot be "+
			"overridden by a stale one in a file")
	flags.Var(&valuesFiles, "f", "shorthand for --values")
	timeout := flags.Duration("timeout", 5*time.Minute, "wait timeout for pod readiness")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: install accepts flags only, no positional arguments", errUsage)
	}

	cfg.Timeout = *timeout
	cfg.BinaryVersion = version
	cfg.SecretStoreUndecided = !cfg.InstallSecretStore && !cfg.InstallSecretStoreDev
	cfg.CredentialedSecretPaths = credentialedSecretPaths
	cfg.TLSExtraDNSNames = tlsExtraDNSNames
	cfg.TLSExtraIPs = tlsExtraIPs
	cfg.ValuesFiles = valuesFiles

	return installer.Execute(ctx, cfg, installer.InstallDeps{
		Output: output,
		Input:  input,
	})
}
