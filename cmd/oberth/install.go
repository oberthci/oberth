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

type installFlagOptions struct {
	answersPath              *string
	yes                      *bool
	verify                   *bool
	jsonOutput               *bool
	timeout                  *time.Duration
	argoParallelism          *int
	argoNamespaceParallelism *int
	alsoUpstreams            stringList
	credentialedSecretPaths  stringList
	argoSyncLimits           stringList
	tlsExtraDNSNames         stringList
	tlsExtraIPs              stringList
	tlsRemoveDNSNames        stringList
	tlsRemoveIPs             stringList
	valuesFiles              stringList
}

func installFlags() (*flag.FlagSet, *installer.Config, *installFlagOptions) {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.Config
	var options installFlagOptions
	options.answersPath = flags.String("answers", "", "version 1 YAML/JSON install answers; explicit flags override scalar answers")
	options.yes = flags.Bool("yes", false, "confirm the reviewed install on the selected kubeconfig context")
	options.verify = flags.Bool("verify", false, "check an existing installation without changing it")
	options.jsonOutput = flags.Bool("json", false, "emit the public install result and verification checks as JSON")
	flags.StringVar(&cfg.AdminSSHPublicKeyPath, "admin-ssh-public-key", "", "local SSH public key for the first admin uplink (file or - for stdin; never a private key)")
	flags.StringVar(&cfg.SecretsAgeRecipient, "secrets-age-recipient", "", "age recipient for encrypting --secrets-file; requires age on PATH")
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig file for an existing cluster")
	flags.StringVar(&cfg.ContextName, "context", "", "kubeconfig context to install on (any cluster it reaches; required when the kubeconfig has more than one context)")
	flags.BoolVar(&cfg.CreateKind, "create-kind", false, "create or use the local oberth kind cluster")
	flags.StringVar(&cfg.ForgeURL, "upstream-url", "", "organization upstream base URL, e.g. ssh://git@github.com/oberthci")
	flags.StringVar(&cfg.ForgeType, "forge", "", "upstream name (github, codeberg, or gitlab)")
	flags.StringVar(&cfg.ForgeOrg, "organization", "", "upstream organization")
	flags.StringVar(&cfg.ExpectedHostFingerprint, "expected-host-fingerprint", "",
		"SSH host-key fingerprint (SHA256:..., from ssh-keygen -lf or the forge's documentation) of the first upstream's host, "+
			"so a non-interactive install trusts a forge whose key Oberth does not ship")
	flags.StringVar(&cfg.UplinkIdentity, "uplink-identity", "", "workstation identity (default: user@host)")
	flags.StringVar(&cfg.SSHPublicKeyPath, "ssh-public-key", "",
		"workstation SSH public key path (default without a terminal: ~/.ssh/id_ed25519.pub, else ~/.ssh/oberth_ed25519.pub, generated on the first run)")
	flags.StringVar(&cfg.UpstreamPrivateKeyPath, "upstream-key-file", "", "existing work SSH private key to import into OpenBao")
	flags.StringVar(&cfg.ClientAccess, "client-access", "",
		"write this machine's client files without asking: both, cli, mcp, or none")
	flags.BoolVar(&cfg.ReplaceClientFiles, "replace-client-files", false,
		"without a terminal: replace this machine's client files (~/.config/oberth) when they exist with other content, "+
			"such as another server's; by default they are kept and nothing is written")
	flags.StringVar(&cfg.ClientHost, "client-host", "",
		"address clients use to reach this server, a DNS name or IP (default: the Kubernetes API host, or localhost); "+
			"client files, SSH config and printed URLs use it and the certificate covers it")
	flags.StringVar(&cfg.ClientSSHPort, "client-ssh-port", "",
		"git SSH port clients reach the server on, when it is not the Service's (default: the NodePort 30022, or the Service port); "+
			"client files, SSH config and printed commands use it")
	flags.StringVar(&cfg.ClientHTTPSPort, "client-https-port", "",
		"HTTPS port clients reach the server on, when it is not the Service's (default: the NodePort 30443, or the Service port); "+
			"client files, the push banner and printed URLs use it")
	flags.Var(&options.alsoUpstreams, "also-upstream",
		"another organization to connect with the same upstream key, host/organization or ssh:// URL (repeatable), "+
			"e.g. codeberg.org/acme")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "print plan without making changes")
	flags.BoolVar(&cfg.Upgrade, "upgrade", false,
		"force helm reconciliation of existing releases at the installed Oberth version; "+
			"version changes of an existing Oberth release are refused here and go through oberth upgrade")
	flags.BoolVar(&cfg.InstallSecretStore, "install-secretstore", false,
		"also install a production-mode OpenBao (persistent storage; verified TLS bootstrapped directly on its PVC; initialized and unsealed via kubectl exec, "+
			"root token and unseal key shown once, on the terminal or in --secrets-file) and configure it as Oberth's release secret store")
	flags.StringVar(&cfg.SecretsFile, "secrets-file", "",
		"write the credentials this install creates once (OpenBao root token and unseal key, the first uplink's bearer token) "+
			"to this new file (mode 0600; refused when it exists or lies in a git worktree) and print only its path and SHA-256; "+
			"required when the output is not a terminal")
	flags.BoolVar(&cfg.InstallSecretStoreDev, "install-secretstore-dev", false,
		"also install a dev-mode OpenBao (in-memory, auto-unsealed, well-known root token) and configure it "+
			"as Oberth's release secret store — evaluation only")
	flags.BoolVar(&cfg.AssistEnabled, "assist", false,
		"enable agent-assisted continuation (chart value assist.enabled, off by default; later: oberth upgrade --set assist.enabled=true); see docs/assisted-continuation.md")
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
	flags.StringVar(&cfg.ChartPath, "chart", "",
		"install the Oberth chart from this local directory or archive instead of the signed release (development only: not verified)")
	addReleaseSourceFlags(flags, &cfg.Release.Dir, &cfg.Release.Origin, &cfg.Release.ChartRepository)
	flags.StringVar(&cfg.UtilityImage, "utility-image", "", "file-tool image for cache preparation and database helpers (air-gapped mirror)")
	flags.StringVar(&cfg.ImageRef, "image", "",
		"server image the chart deploys, overriding the chart default (used with --chart for local iteration; without --chart it must be the signed release's image)")
	flags.StringVar(&cfg.OpenBaoChartVersion, "openbao-chart-version", "", "OpenBao chart version")
	flags.StringVar(&cfg.RekorChartVersion, "rekor-chart-version", "", "Rekor chart version")
	flags.StringVar(&cfg.ArgoChartVersion, "argo-chart-version", "", "argo-workflows chart version")
	flags.StringVar(&cfg.ArgoControllerProfile, "argo-controller-profile", "", "install the verified nonroot-static-v1 controller profile (requires the managed pinned chart)")
	flags.StringVar(&cfg.ArgoValuesReviewed, "argo-values-reviewed", "", argoValuesReviewedHelp)
	flags.StringVar(&cfg.ArgoVaultAddress, "argo-vault-address", "",
		"OpenBao/Vault base URL injected into credentialed pipeline containers as VAULT_ADDR (https only)")
	flags.StringVar(&cfg.PushBannerURL, "push-banner-url", "", "push feedback URL; omitted preserves the live URL, explicit empty clears it")
	flags.StringVar(&cfg.ServiceType, "service-type", "",
		"Oberth Service type: NodePort (default for a fresh install; clients use NodePorts 30022/30443), "+
			"LoadBalancer (on k3s, servicelb also takes host ports 2222 and 443 on the node) or ClusterIP; "+
			"an upgrade keeps the release's current type unless this is set")
	flags.StringVar(&cfg.NetworkPolicy, "network-policy", "",
		"pipeline egress NetworkPolicy: auto (default: enable, including k3s; preserve existing values), "+
			"true (always enable), false (always disable)")
	flags.StringVar(&cfg.ArgoVaultCredentialedRole, "argo-vault-credentialed-role", "",
		"Kubernetes-auth role credentialed pipelines log in with; bind it to exactly "+
			"(--argo-namespace, the credentialed ServiceAccount)")
	options.argoParallelism = flags.Int("argo-parallelism", -1,
		"Argo controller parallelism: how many pipeline Workflows run at once, 0 for no limit; "+
			"the excess waits Pending as postponed (default: keep the release's value, none on a fresh install)")
	options.argoNamespaceParallelism = flags.Int("argo-namespace-parallelism", -1,
		"Argo controller namespaceParallelism for the pipeline namespace, 0 for no limit (default: keep the release's value)")
	flags.Var(&options.argoSyncLimits, "argo-sync-limit",
		"semaphore limit key=N in the ConfigMap oberth-sync-limits, rendered by the argo-workflows release (repeatable; "+
			"the set given replaces the previous one; see docs/where-limits-live.md)")
	flags.Var(&options.credentialedSecretPaths, "credentialed-secret-path",
		"full KV data path approved in the secret-access table (repeatable, exactly the string "+
			"`oberth access allow` records, e.g. oberth/data/release/cosign-secret); synced into the "+
			"RELEASE-tier credentialed Vault policy as an exact read grant when --install-secretstore "+
			"reconciles the store — the branch-tier ci-secrets policy never receives grants")
	flags.Var(&options.tlsExtraDNSNames, "tls-extra-dns-name",
		"additional DNS name the generated server certificate is issued for (repeatable), beyond the "+
			"in-cluster names; a client reaching the server on any other name hits a hostname mismatch "+
			"that no trust anchor can fix. A macOS kind install adds localhost automatically")
	flags.Var(&options.tlsRemoveDNSNames, "tls-remove-dns-name", "remove a permitted DNS name; refused while the served certificate contains it (repeatable)")
	flags.Var(&options.tlsRemoveIPs, "tls-remove-ip", "remove a permitted IP; refused while the served certificate contains it (repeatable)")
	flags.Var(&options.tlsExtraIPs, "tls-extra-ip",
		"additional IP address the generated server certificate is issued for (repeatable); an address "+
			"reached as an IP needs an IP SAN, because a DNS name never matches one. A macOS kind "+
			"install adds 127.0.0.1 automatically")
	flags.Var(&options.valuesFiles, "values",
		"helm values file applied to the Oberth release (repeatable, applied in order); "+
			"the installer's own settings still win, so a value it computed cannot be "+
			"overridden by a stale one in a file")
	flags.Var(&options.valuesFiles, "f", "shorthand for --values")
	options.timeout = flags.Duration("timeout", installer.DefaultTimeout, "wait timeout for pod readiness")

	return flags, &cfg, &options
}

func runInstall(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags, cfg, options := installFlags()
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if *options.answersPath != "" {
		loaded, err := installer.ReadAnswers(*options.answersPath)
		if err != nil {
			return usageError(err)
		}
		*cfg = loaded
		options.alsoUpstreams = append(stringList(nil), cfg.AlsoUpstreams...)
		options.credentialedSecretPaths = append(stringList(nil), cfg.CredentialedSecretPaths...)
		options.tlsExtraDNSNames = append(stringList(nil), cfg.TLSExtraDNSNames...)
		options.tlsExtraIPs = append(stringList(nil), cfg.TLSExtraIPs...)
		options.tlsRemoveDNSNames = append(stringList(nil), cfg.TLSRemoveDNSNames...)
		options.tlsRemoveIPs = append(stringList(nil), cfg.TLSRemoveIPs...)
		options.valuesFiles = append(stringList(nil), cfg.ValuesFiles...)
		options.argoSyncLimits = nil
		*options.timeout = cfg.Timeout
		if *options.timeout == 0 {
			*options.timeout = installer.DefaultTimeout
		}
		if err := flags.Parse(arguments); err != nil {
			return usageError(err)
		}
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: install accepts flags only, no positional arguments", errUsage)
	}

	flags.Visit(func(f *flag.Flag) {
		if f.Name == "push-banner-url" {
			cfg.PushBannerURLSet = true
		}
		if f.Name == "accept-witness-genesis" {
			cfg.AcceptWitnessGenesisSet = true
		}
	})
	cfg.Timeout = *options.timeout
	cfg.BinaryVersion = version
	if *options.answersPath == "" {
		cfg.SecretStoreUndecided = !cfg.InstallSecretStore && !cfg.InstallSecretStoreDev
	}
	cfg.CredentialedSecretPaths = options.credentialedSecretPaths
	cfg.TLSExtraDNSNames = options.tlsExtraDNSNames
	cfg.TLSExtraIPs = options.tlsExtraIPs
	cfg.TLSRemoveDNSNames = options.tlsRemoveDNSNames
	cfg.TLSRemoveIPs = options.tlsRemoveIPs
	cfg.AlsoUpstreams = options.alsoUpstreams
	cfg.ValuesFiles = options.valuesFiles
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "argo-parallelism":
			cfg.ArgoParallelism = options.argoParallelism
		case "argo-namespace-parallelism":
			cfg.ArgoNamespaceParallelism = options.argoNamespaceParallelism
		}
	})
	if len(options.argoSyncLimits) > 0 {
		cfg.ArgoSyncLimits = map[string]int{}
		for _, value := range options.argoSyncLimits {
			key, limit, err := installer.ParseArgoSyncLimit(value)
			if err != nil {
				return usageError(err)
			}
			cfg.ArgoSyncLimits[key] = limit
		}
	}

	deps := installer.InstallDeps{Output: output, Input: input}
	if *options.answersPath != "" {
		deps.IsTerminal = func() bool { return false }
	}
	if *options.verify {
		return installer.PrintVerification(ctx, *cfg, deps, *options.jsonOutput)
	}
	if err := cfg.Validate(); err != nil {
		return usageError(err)
	}
	if !*options.jsonOutput {
		installer.PrintReview(output, *cfg)
	}
	if !cfg.DryRun && !*options.yes {
		if *options.jsonOutput {
			if err := installer.PrintReviewJSON(output, *cfg); err != nil {
				return err
			}
		}
		return usageError(errors.New("review the selected context and install answers, then pass --yes to confirm install"))
	}
	if *options.jsonOutput {
		return installer.ExecuteJSON(ctx, *cfg, deps)
	}
	return installer.Execute(ctx, *cfg, deps)
}
