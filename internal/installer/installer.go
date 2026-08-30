// Package installer implements the oberth install command that bootstraps
// Oberth into a local Kubernetes cluster for dev/evaluation — plus, on
// request, an OpenBao secret store in production mode (--install-secretstore)
// or dev mode (--install-secretstore-dev) and a local Rekor transparency-log
// stack for the audit witness (--install-rekor). All OpenBao administration
// runs through `kubectl exec` inside the OpenBao pod: no port-forward, no
// local bao/vault CLI, and no OpenBao API exposure outside the cluster.
package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// DefaultOpenBaoNamespace is the namespace for the dev OpenBao install.
	DefaultOpenBaoNamespace = "openbao"
	// DefaultNamespace is the namespace for Oberth.
	DefaultNamespace = "oberth"
	// DefaultTimeout is the default wait-for-ready deadline.
	DefaultTimeout = 5 * time.Minute
	// DefaultOpenBaoChartVersion is the pinned OpenBao chart version.
	// Bump deliberately and re-verify the dev-mode install flow; never float
	// to latest.
	DefaultOpenBaoChartVersion = "0.28.6"

	// OpenBaoHelmRepoURL is the canonical OpenBao Helm chart repository.
	OpenBaoHelmRepoURL = "https://openbao.github.io/openbao-helm"
	// OberthHelmRepoURL is the canonical Oberth Helm chart repository — the
	// oberth-specific index track, not the root charts.cloudtaser.io index.
	OberthHelmRepoURL = "https://charts.cloudtaser.io/oberth"

	openBaoRepoName = "openbao"
	oberthRepoName  = "oberth-charts"

	// SigstoreHelmRepoURL is the canonical Sigstore Helm chart repository,
	// serving the rekor umbrella chart (bundled trillian + createtree).
	SigstoreHelmRepoURL = "https://sigstore.github.io/helm-charts"
	// DefaultRekorNamespace is the namespace for the local Rekor stack.
	DefaultRekorNamespace = "rekor"
	// DefaultRekorChartVersion is the pinned sigstore/rekor umbrella chart
	// version (appVersion v1.5.3; bundles trillian chart 0.3.16). Bump
	// deliberately and re-verify the local witness flow against
	// RekorHelmValuesYAML; never float to latest.
	DefaultRekorChartVersion = "1.8.3"

	sigstoreRepoName = "sigstore"

	// rekorSignerSecretName/Key hold the persistent Rekor log signing key
	// (PKCS#8 ECDSA P-256 PEM) consumed via the chart's
	// server.signerFileSecretOptions. The key is minted once and reused on
	// every subsequent install: it IS the local log's identity, and rotating
	// it would invalidate every previously published witness entry.
	rekorSignerSecretName = "rekor-signer" // #nosec G101 — Kubernetes Secret NAME (an identifier), not credential material.
	rekorSignerSecretKey  = "private.pem"
	rekorSignerMountPath  = "/etc/rekor/signer"

	// devRootToken is the OpenBao dev-mode default root token — well-known,
	// documented, and printed to the operator after a dev-mode install so
	// release secrets can be seeded (bao kv put) without any lookup.
	devRootToken = "root" // #nosec G101 — well-known dev-mode default, not a credential.

	defaultAuthMount      = "kubernetes"
	defaultRole           = "oberth-ci"
	defaultPolicy         = "oberth-ci"
	defaultKVPrefix       = "oberth"
	defaultTransitMount   = "oberth-transit"
	defaultTransitKey     = "trusted-plan-artifacts"
	defaultServiceAccount = "oberth"

	// Credentialed tier: release templates with approved secrets.
	defaultCredentialedRole   = "oberth-argo-credentialed" // #nosec G101 -- an OpenBao role name, not a credential.
	defaultCredentialedPolicy = "oberth-argo-credentialed" // #nosec G101 -- an OpenBao policy name, not a credential.
	// #nosec G101 -- a ServiceAccount name, not a credential.
	defaultCredentialedServiceAccount = "oberth-argo-credentialed"

	// CI-secrets tier: CI templates with approved upstream-scoped secrets.
	// A separate identity from the credentialed tier on purpose — its policy
	// covers the upstream subtree only and never receives approval-table
	// grants, so a branch push can never present an identity the release
	// role accepts (issue #200).
	defaultCISecretsRole   = "oberth-argo-ci-secrets" // #nosec G101 -- an OpenBao role name, not a credential.
	defaultCISecretsPolicy = "oberth-argo-ci-secrets" // #nosec G101 -- an OpenBao policy name, not a credential.
	// #nosec G101 -- a ServiceAccount name, not a credential.
	defaultCISecretsServiceAccount = "oberth-argo-ci-secrets"
)

// Config holds options for the install command.
type Config struct {
	Dev        bool
	Production bool
	DryRun     bool
	Yes        bool
	Upgrade    bool
	// InstallSecretStore additionally installs a PRODUCTION-mode OpenBao —
	// standalone server with persistent storage — then initializes it
	// (1 key share, threshold 1) and unseals it via kubectl exec, prints the
	// root token and unseal key exactly once, and configures it as Oberth's
	// release secret store (--install-secretstore).
	InstallSecretStore bool
	// InstallSecretStoreDev additionally installs a DEV-mode OpenBao —
	// in-memory, auto-unsealed, well-known root token — and configures it as
	// Oberth's release secret store (--install-secretstore-dev). Evaluation
	// only: state does not survive a pod restart.
	InstallSecretStoreDev bool
	// SecretStoreUndecided is set by the CLI when neither
	// --install-secretstore nor --install-secretstore-dev was passed. When
	// true, Run proposes the choice interactively (or defaults to production
	// mode in a non-interactive session).
	SecretStoreUndecided bool
	// InstallRekor additionally installs a local Rekor transparency-log
	// stack — MySQL + Trillian log server/signer + Rekor server — as the
	// audit witness (--install-rekor). Independent of the secret store.
	InstallRekor bool
	// SkipArgo opts out of installing the Argo Workflows controller
	// (--skip-argo-workflows), for a cluster that already runs one watching
	// ArgoNamespace. It does not make pipelines execute any other way: Argo is
	// the execution engine, so the install still binds Oberth to that
	// namespace and still verifies a controller is really there.
	SkipArgo                  bool
	OpenBaoNamespace          string
	Namespace                 string
	RekorNamespace            string
	ArgoNamespace             string
	ChartVersion              string
	OpenBaoChartVersion       string
	RekorChartVersion         string
	ArgoChartVersion          string
	ArgoVaultAddress          string
	ArgoVaultCredentialedRole string
	// CredentialedSecretPaths are exact secret paths approved through the
	// approval table, included in the credentialed Vault policy as
	// exact-path grants alongside the upstream/* wildcard. Populated from
	// the approval table when the installer syncs the policy to OpenBao.
	CredentialedSecretPaths []string
	// PerRepoIdentities describes per-repo Vault identities to create.
	// Each entry results in a ServiceAccount (via the chart), a Vault
	// policy, and a Vault role (via ConfigurePerRepoIdentities). The
	// installer populates this from the repo registry + approval table.
	PerRepoIdentities []PerRepoIdentity

	// TLSExtraDNSNames and TLSExtraIPs are addresses the generated server
	// certificate must carry beyond the four in-cluster names. A client
	// reaching the server on any other address gets a hostname mismatch, which
	// is the one TLS failure no trust anchor can repair, so naming the address
	// is a deployment decision rather than something a client works around.
	// Ignored when the chart adopts an existing TLS Secret.
	TLSExtraDNSNames []string
	TLSExtraIPs      []string

	// ValuesFiles are helm values files applied to the Oberth release, in the
	// order given, so a deployment can express its own settings at install time
	// rather than reconciling afterwards with a second command. Applied before
	// the installer's own --set flags, which therefore still win: a value this
	// process computed, such as the address of the secret store it just
	// created, must not be replaced by a stale one in a file.
	ValuesFiles []string
	// NetworkPolicy controls pipeline egress NetworkPolicy enforcement:
	// "auto" (default) enables on all CNIs except k3s's built-in kube-router
	// (which has a DNAT incompatibility), "true" forces on, "false" forces off.
	NetworkPolicy string
	// ShellProfile decides whether the install appends the sourcing line to
	// the operator's shell profile: "yes", "no", or empty to ask when there
	// is a terminal to ask on. Empty in a script means no: an install that was
	// not told to edit a file the caller never mentioned does not edit it.
	ShellProfile string

	// ChartPath installs the Oberth chart from a local directory or archive
	// instead of the published repository (--chart). This is the loop the
	// rollout depends on: build the server image, install the working tree's
	// chart, push, watch it run, change something, repeat — without a release
	// in between.
	ChartPath string
	// ImageRef overrides the server image the chart deploys (--image), for the
	// same loop. A tag that exists only in the node's image store is the normal
	// case here, so the chart's IfNotPresent pull policy is what makes it work.
	ImageRef      string
	Timeout       time.Duration
	BinaryVersion string
	// AcceptWitnessGenesis is the one-shot "<auditID>:<sha256hex>" acknowledgment
	// forwarded to auditAnchor.acceptWitnessGenesis when --install-rekor retrofits
	// an existing deployment that already has audit history.
	AcceptWitnessGenesis string
}

// wantsSecretStore reports whether either secret-store mode was requested.
func (cfg Config) wantsSecretStore() bool {
	return cfg.InstallSecretStore || cfg.InstallSecretStoreDev
}

// promptSecretStoreChoice proposes installing OpenBao when no explicit
// --install-secretstore or --install-secretstore-dev flag was passed. In an
// interactive terminal it presents a three-option menu; in a non-interactive
// session it skips with guidance (credentials printed during production
// setup must never land in CI logs or transcripts unsolicited).
func promptSecretStoreChoice(ctx context.Context, cfg *Config, deps Deps) error {
	interactive := deps.IsTerminal != nil && deps.IsTerminal() && deps.Input != nil
	if !interactive {
		_, _ = fmt.Fprintln(deps.Output, "No secret store selected. Re-run with --install-secretstore to enable release secret management.")
		return nil
	}

	_, _ = fmt.Fprintln(deps.Output, "\nInstall OpenBao for release secret management?")
	_, _ = fmt.Fprintln(deps.Output, "  [1] Production mode (persistent storage, verified TLS) (Recommended)")
	_, _ = fmt.Fprintln(deps.Output, "  [2] Skip — no secret store (branch CI only, releases will not work)")

	var chosen bool
	for attempt := 0; attempt < 3; attempt++ {
		_, _ = fmt.Fprint(deps.Output, "Choice [1]: ")
		answer, err := readLine(ctx, deps.Input)
		if err != nil {
			return fmt.Errorf("read secret store choice: %w", err)
		}
		switch strings.TrimSpace(answer) {
		case "", "1":
			cfg.InstallSecretStore = true
			chosen = true
		case "2":
			_, _ = fmt.Fprintln(deps.Output, "Warning: releases will not work without a secret store. Re-run with --install-secretstore later to add one.")
			return nil
		default:
			_, _ = fmt.Fprintln(deps.Output, "Please enter 1 or 2.")
			continue
		}
		break
	}
	if !chosen {
		_, _ = fmt.Fprintln(deps.Output, "Warning: releases will not work without a secret store. Re-run with --install-secretstore later to add one.")
		return nil
	}

	ns, err := promptValidNamespace(ctx, deps, "OpenBao namespace", cfg.OpenBaoNamespace)
	if err != nil {
		return err
	}
	cfg.OpenBaoNamespace = ns

	ns, err = promptValidNamespace(ctx, deps, "Argo pipeline namespace", cfg.ArgoNamespace)
	if err != nil {
		return err
	}
	cfg.ArgoNamespace = ns

	if cfg.ArgoNamespace == cfg.Namespace {
		return fmt.Errorf("argo pipeline namespace %q must differ from the Oberth namespace: "+
			"OpenBao Kubernetes-auth roles bind (namespace, ServiceAccount) pairs, "+
			"so pipeline identities must not sit beside server ones", cfg.ArgoNamespace)
	}

	return nil
}

// promptValidNamespace prompts for a Kubernetes namespace, validates it as a
// DNS-1123 label, and re-prompts on invalid input (up to 3 attempts). An
// empty answer or EOF accepts the default. Real read errors are propagated.
func promptValidNamespace(ctx context.Context, deps Deps, label, defaultValue string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		_, _ = fmt.Fprintf(deps.Output, "%s [%s]: ", label, defaultValue)
		ns, err := readLine(ctx, deps.Input)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return defaultValue, nil
			}
			return "", fmt.Errorf("read %s: %w", strings.ToLower(label), err)
		}
		trimmed := strings.TrimSpace(ns)
		if trimmed == "" {
			return defaultValue, nil
		}
		if isValidDNS1123Label(trimmed) {
			return trimmed, nil
		}
		_, _ = fmt.Fprintln(deps.Output, "Must be a valid DNS-1123 label (lowercase alphanumeric and '-', 1-63 chars, must start and end with alphanumeric).")
	}
	return defaultValue, nil
}

// isValidDNS1123Label reports whether s is a valid DNS-1123 label:
// lowercase ASCII alphanumeric and '-', 1-63 characters, starting and
// ending with an alphanumeric character.
func isValidDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// Deps holds injectable dependencies for testing.
type Deps struct {
	Output         io.Writer
	Input          io.Reader
	RunHelm        func(ctx context.Context, args []string) ([]byte, error)
	RunCommand     CommandRunner
	RunInteractive InteractiveRunner
	// PodLogs returns one pod's bounded bootstrap output. When nil, the
	// Kubernetes REST client is used directly.
	PodLogs    func(ctx context.Context, namespace, pod string) ([]byte, error)
	IsTerminal func() bool
	// HomeDir resolves the operator's home directory. Injectable because the
	// shell-profile step writes inside it, and a test that reaches the real
	// one edits the developer's own machine.
	//
	// Nil means os.UserHomeDir, which is correct everywhere except a test.
	HomeDir func() (string, error)
	// LookPath resolves a host binary, so a step that adapts to what is
	// installed can be tested without depending on the test machine's PATH.
	// Nil selects exec.LookPath.
	LookPath        func(string) (string, error)
	KubeClient      kubernetes.Interface
	RestConfig      *rest.Config
	ContextName     string
	KindClusterName string
	// PollInterval overrides the retry cadence of in-cluster status polls
	// (OpenBao status, in-pod upstream listing) for tests; zero selects the
	// production default of 2s.
	PollInterval time.Duration
	// ReadPassword reads a line of input with terminal echo disabled. The
	// production default uses x/term.ReadPassword on the stdin fd; tests
	// inject a func that returns a scripted token. Nil means no echo-off
	// capability (non-interactive session).
	ReadPassword func() ([]byte, error)
	// MakeRaw puts the terminal into raw mode and returns a function that
	// restores the previous state. The production default uses
	// x/term.MakeRaw on the stdin fd; tests inject a no-op pair or leave
	// it nil to exercise the text-based fallback. Nil means no raw-mode
	// capability (non-interactive session or pipe).
	MakeRaw func() (restore func(), err error)
}

// ClusterInfo describes the target Kubernetes cluster.
type ClusterInfo struct {
	Context string
	Server  string
	Engine  string // "k3s", "kind", "k0s", "minikube", "unknown"
	IsLocal bool
}

// OpenBaoResult describes the outcome of the OpenBao install phase.
type OpenBaoResult struct {
	AlreadyInstalled bool
	// Upgraded reports that an existing release was moved to TargetVersion
	// (or force-reconciled with --upgrade) instead of freshly installed.
	Upgraded bool
	// InstalledVersion is the chart version found before any action ("" when
	// the release was absent or its version could not be determined).
	InstalledVersion string
	// TargetVersion is the chart version the install aimed at.
	TargetVersion  string
	Namespace      string
	ServiceAddress string
	// CACertPEM is the public installer-managed CA used to verify the
	// production OpenBao listener. It is empty in explicit dev mode.
	CACertPEM string
	// TrustedTransitVerified is set only after the production setup has
	// positively provisioned or verified the exact managed Transit mount, key,
	// policy, and Kubernetes role. It gates runtime enablement.
	TrustedTransitVerified bool
}

// SecretStoreResult describes the outcome of secretstore configuration.
type SecretStoreResult struct {
	AuthMountConfigured    bool
	TransitMountEnabled    bool
	TransitKeyCreated      bool
	PolicyWritten          bool
	RoleCreated            bool
	TrustedTransitVerified bool
	Skipped                bool
	// Items collects the provisioned/verified objects for the config grid.
	Items []configItem
}

// OberthResult describes the outcome of the Oberth install phase.
type OberthResult struct {
	AlreadyInstalled bool
	// Upgraded reports that an existing release was moved to TargetVersion
	// (or reconciled to apply explicitly requested values) instead of freshly
	// installed.
	Upgraded bool
	// InstalledVersion is the chart version found before any action ("" when
	// the release was absent or its version could not be determined).
	InstalledVersion string
	// TargetVersion is the chart version the install aimed at ("" when the
	// binary carries no release version and no --chart-version was given).
	TargetVersion string
	Namespace     string
}

// RekorResult describes the outcome of the local Rekor stack install phase.
type RekorResult struct {
	AlreadyInstalled bool
	// Upgraded reports that an existing release was moved to TargetVersion
	// (or force-reconciled with --upgrade) instead of freshly installed.
	Upgraded bool
	// InstalledVersion is the chart version found before any action ("" when
	// the release was absent or its version could not be determined).
	InstalledVersion string
	// TargetVersion is the chart version the install aimed at.
	TargetVersion string
	Namespace     string
	// ServiceAddress is the in-cluster base URL of the Rekor server
	// (chart service port 80 → container 3000). The installer opts Oberth into
	// its explicit development-only HTTP transport for this in-cluster URL.
	ServiceAddress string
	// LogPublicKeyPEM is the PKIX PEM public key of the local log's
	// persistent file signer — the value the Oberth chart pins via
	// auditAnchor.rekorPublicKey.
	LogPublicKeyPEM string
}

// Validate checks Config for invalid flag combinations and applies defaults.
func (cfg *Config) Validate() error {
	if cfg.Dev && cfg.Production {
		return errors.New("--dev and --production are mutually exclusive")
	}
	if cfg.Production {
		return errors.New("production mode is not implemented yet; tracked in BACKLOG.md")
	}
	if cfg.InstallSecretStore && cfg.InstallSecretStoreDev {
		return errors.New("--install-secretstore and --install-secretstore-dev are mutually exclusive")
	}
	// Refused rather than read as "no": a typo that silently declines is a
	// typo that produces the exact behavior the flag was passed to avoid.
	switch strings.ToLower(strings.TrimSpace(cfg.ShellProfile)) {
	case "", "yes", "no":
		cfg.ShellProfile = strings.ToLower(strings.TrimSpace(cfg.ShellProfile))
	default:
		return fmt.Errorf("--shell-profile %q is not yes or no", cfg.ShellProfile)
	}
	if !cfg.Dev {
		cfg.Dev = true
	}
	if cfg.OpenBaoNamespace == "" {
		cfg.OpenBaoNamespace = DefaultOpenBaoNamespace
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.OpenBaoChartVersion == "" {
		cfg.OpenBaoChartVersion = DefaultOpenBaoChartVersion
	}
	if cfg.RekorNamespace == "" {
		cfg.RekorNamespace = DefaultRekorNamespace
	}
	if cfg.RekorChartVersion == "" {
		cfg.RekorChartVersion = DefaultRekorChartVersion
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = DefaultArgoNamespace
	}
	if cfg.ArgoChartVersion == "" {
		cfg.ArgoChartVersion = DefaultArgoChartVersion
	}
	if cfg.ArgoNamespace == cfg.Namespace {
		return fmt.Errorf("--argo-namespace %q must differ from --namespace: OpenBao Kubernetes-auth "+
			"roles bind (namespace, ServiceAccount) pairs, so pipeline identities must not sit beside "+
			"server ones", cfg.ArgoNamespace)
	}
	// A plaintext VAULT_ADDR would be injected into every release-tier
	// container, which is where a real credential is exchanged.
	if address := strings.TrimSpace(cfg.ArgoVaultAddress); address != "" && !strings.HasPrefix(address, "https://") {
		return fmt.Errorf("--argo-vault-address must be an https:// URL, got %q", address)
	}
	switch cfg.NetworkPolicy {
	case "":
		cfg.NetworkPolicy = "auto"
	case "auto", "true", "false":
		// valid
	default:
		return fmt.Errorf("--network-policy must be auto, true, or false, got %q", cfg.NetworkPolicy)
	}
	if cfg.AcceptWitnessGenesis != "" && !cfg.InstallRekor {
		return errors.New("--accept-witness-genesis requires --install-rekor")
	}
	if cfg.AcceptWitnessGenesis != "" {
		wgPattern := regexp.MustCompile(`^[1-9][0-9]{0,18}:[0-9a-f]{64}$`)
		if !wgPattern.MatchString(cfg.AcceptWitnessGenesis) {
			return errors.New("--accept-witness-genesis must be the exact audit chain head as <auditID>:<sha256hex>")
		}
	}
	if cfg.ChartVersion == "" && cfg.BinaryVersion != "" && cfg.BinaryVersion != "dev" {
		cfg.ChartVersion = cfg.BinaryVersion
	}
	return nil
}

// Run executes the full install flow.
func Run(ctx context.Context, cfg Config, deps Deps) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	cluster, err := DetectCluster(ctx, deps)
	if err != nil {
		return fmt.Errorf("detect cluster: %w", err)
	}
	if deps.KindClusterName != "" {
		cluster.Engine = "kind"
	} else if cluster.Engine == "kind" {
		clusterName, ok := kindNameFromContext(cluster.Context)
		if !ok {
			return fmt.Errorf("kind context %q does not use the expected kind-<name> format", cluster.Context)
		}
		deps.KindClusterName = clusterName
	}
	// Cluster info line omitted — the kind/k3s creation messages already
	// communicate the cluster identity; repeating it adds noise.

	if !cluster.IsLocal && !cfg.Yes {
		return fmt.Errorf("context %q does not appear to be a local cluster (server: %s); "+
			"oberth install is a bootstrap command that creates a dev secret store — "+
			"use --yes to proceed or switch to a local context", cluster.Context, cluster.Server)
	}
	if !cluster.IsLocal && cfg.Yes {
		_, _ = fmt.Fprintf(deps.Output, "WARNING: %q does not appear to be a local cluster; proceeding because --yes was set\n", cluster.Context)
	}

	if cfg.InstallSecretStoreDev {
		_, _ = fmt.Fprintln(deps.Output, "EVALUATION MODE — not for production. Dev OpenBao state does not survive pod restart.")
	}

	// Resolve auto network policy based on cluster engine before the dry-run
	// plan so it accurately reflects the install decision.
	if cfg.NetworkPolicy == "auto" {
		if cluster.Engine == "k3s" {
			cfg.NetworkPolicy = "false"
		} else {
			cfg.NetworkPolicy = "true"
		}
	}

	// When no explicit secret-store flag was passed, resolve the decision
	// before the dry-run plan so it accurately reflects the install.
	if cfg.SecretStoreUndecided {
		if err := promptSecretStoreChoice(ctx, &cfg, deps); err != nil {
			return err
		}
		if cfg.InstallSecretStoreDev {
			_, _ = fmt.Fprintln(deps.Output, "EVALUATION MODE — not for production. Dev OpenBao state does not survive pod restart.")
		}
	}

	if cfg.DryRun {
		return printDryRunPlan(cfg, deps.Output, cluster)
	}

	// Before anything is installed: naming an address the kept TLS Secret will
	// not gain is silent otherwise, and surfaces weeks later as a hostname
	// mismatch nobody connects back to this run.
	warnCertificateNamesWillNotTakeEffect(deps.Output, cfg,
		certificateNamesNotYetIssued(ctx, cfg, deps))

	w := deps.Output
	color := isColor(deps)

	_, _ = fmt.Fprintf(w, "oberth install %s\n", displayVersion(cfg.BinaryVersion))

	var creds heldCredentials

	// Safety net: if any later step errors out or panics, captured
	// credentials are still printed. Registered before the secret-store
	// setup because a fresh `operator init` can succeed while a later step
	// still fails — after init the credentials exist nowhere but this
	// process, and losing them locks the store permanently.
	defer creds.flush(w, color)

	var openbao OpenBaoResult
	var rekor RekorResult
	var secretStoreItems []configItem

	// Populate per-repo identities from the running server's database
	// (via exec) when the secret store is being set up. On a fresh
	// install the server pod does not exist yet and the exec fails
	// gracefully, falling back to the ConfigMap — correct, because
	// no repos are registered on a fresh install either. On an
	// upgrade, the database carries qualified "upstream/org/repo"
	// names that match the serve path's identity map exactly.
	if cfg.wantsSecretStore() && len(cfg.PerRepoIdentities) == 0 {
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		produced, produceErr := ProducePerRepoIdentities(ctx, deps.KubeClient, deps.RunCommand, deps.ContextName, ns)
		if produceErr != nil {
			_, _ = fmt.Fprintf(deps.Output, "WARNING: could not read per-repo identities: %v\n", produceErr)
		} else if len(produced) > 0 {
			cfg.PerRepoIdentities = produced
		}
	}

	if cfg.wantsSecretStore() {
		quietDeps := deps
		quietDeps.Output = io.Discard
		openbao, err = InstallOpenBao(ctx, cfg, quietDeps)
		if err != nil {
			return fmt.Errorf("install OpenBao: %w", err)
		}

		if cfg.InstallSecretStoreDev {
			if err := SetupDevSecretStore(ctx, cfg, quietDeps, openbao); err != nil {
				return fmt.Errorf("set up dev secret store: %w", err)
			}
		} else {
			// Production setup captures credentials into the held pool
			// instead of printing them immediately; they are displayed in a
			// merged credential box after the table closes. Credentials are
			// recorded before the error is considered — a failure after a
			// successful `operator init` must still surface them through
			// the deferred flush above.
			credDeps := deps
			credDeps.Output = io.Discard
			configured, err := setupProductionSecretStoreCollect(ctx, cfg, deps, credDeps, openbao, &creds)
			if err != nil {
				return fmt.Errorf("set up production secret store: %w", err)
			}
			openbao.TrustedTransitVerified = configured.TrustedTransitVerified
			secretStoreItems = configured.Items
		}
	}

	// --- Streaming component table: header first, rows as steps complete ---
	tw := newTableWriter(w, color)

	// Register known content so columns auto-size before the header is
	// written. Versions from install results are not yet available, but
	// the secret store items and network policy status are.
	for _, item := range secretStoreItems {
		tw.RegisterContent(item.Name, "secret store", item.Status+" ready")
	}
	if cfg.NetworkPolicy == "false" {
		tw.RegisterContent("Network policy", "disabled (kube-router)", "⚠ warn")
	}
	tw.WriteHeader()

	// Secret store config rows.
	for _, item := range secretStoreItems {
		tw.WriteSensitiveRow(item.Name, "secret store", item.Status+" ready")
	}

	if cfg.InstallRekor {
		rekor, err = InstallRekorStack(ctx, cfg, deps)
		if err != nil {
			tw.WriteFooter()
			return fmt.Errorf("install Rekor stack: %w", err)
		}
		if err := waitForPodReady(ctx, deps, rekor.Namespace,
			"app.kubernetes.io/instance=rekor,app.kubernetes.io/component=server", cfg.Timeout); err != nil {
			tw.WriteFooter()
			return fmt.Errorf("wait for Rekor: %w", err)
		}
		tw.WriteRow("Rekor", displayVersion(rekor.TargetVersion), "✓ deployed")
	}

	// Argo before Oberth: the server binds to a controller, so the controller
	// exists first. A push accepted by a server whose namespace nothing
	// reconciles produces a run that never starts and never fails.
	quietArgoDeps := deps
	quietArgoDeps.Output = io.Discard
	argo, err := InstallArgoWorkflows(ctx, cfg, quietArgoDeps)
	if err != nil {
		tw.WriteFooter()
		return fmt.Errorf("install Argo Workflows: %w", err)
	}
	if err := VerifyArgoController(ctx, cfg, quietArgoDeps); err != nil {
		tw.WriteFooter()
		return err
	}
	if argo.Skipped {
		tw.WriteRow("Execution engine", "existing in "+argo.Namespace, "✓ deployed")
	} else {
		tw.WriteRow("Execution engine", "argo-workflows "+displayVersion(argo.TargetVersion), "✓ deployed")
	}

	quietOberthDeps := deps
	quietOberthDeps.Output = io.Discard
	oberthResult, err := InstallOberth(ctx, cfg, quietOberthDeps, openbao, rekor)
	if err != nil {
		tw.WriteFooter()
		return fmt.Errorf("install Oberth: %w", err)
	}
	tw.WriteRow("Oberth", displayVersion(oberthResult.TargetVersion), "✓ deployed")

	// Network policy warning row.
	if cfg.NetworkPolicy == "false" {
		tw.WriteRow("Network policy", "disabled (kube-router)", "⚠ warn")
	}

	// Close the table visually — AppendRow will erase and redraw the border
	// as onboarding steps complete.
	tw.WriteFooter()

	// A fresh deployment is deliberately NotReady until its first upstream is
	// registered (vault-pre-init pattern), so completion goes through the
	// onboarding-aware finish instead of a blind readiness wait. Onboarding
	// results are appended to the table via AppendRow.
	return FinishInstall(ctx, cfg, deps, tw, &creds)
}

// --- Phase 1: Cluster detection ---

// DetectCluster identifies the current Kubernetes context, API server, and engine.
func DetectCluster(ctx context.Context, deps Deps) (ClusterInfo, error) {
	info := ClusterInfo{
		Context: deps.ContextName,
		Server:  deps.RestConfig.Host,
	}
	info.IsLocal = IsLocalServer(info.Server)

	nodes, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		info.Engine = "unknown"
		return info, nil
	}
	info.Engine = DetectEngine(nodes.Items)
	// Engine detection is informational only. The safety locality decision
	// (whether --yes is required) derives from the API server endpoint, not
	// from the detected engine. A kubelet version that contains "+k3s" on a
	// remote public endpoint must still require --yes, because bootstrap
	// and dev-secret-store flags are destructive on production clusters.
	return info, nil
}

// IsLocalServer reports whether the server URL points to a loopback or RFC1918 address.
func IsLocalServer(serverURL string) bool {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// DetectEngine identifies the Kubernetes engine from node metadata.
func DetectEngine(nodes []corev1.Node) string {
	for _, node := range nodes {
		providerID := node.Spec.ProviderID
		kubeletVersion := node.Status.NodeInfo.KubeletVersion
		labels := node.Labels

		switch {
		case strings.Contains(kubeletVersion, "+k3s"):
			return "k3s"
		case strings.HasPrefix(providerID, "kind://"):
			return "kind"
		case strings.Contains(kubeletVersion, "+k0s"):
			return "k0s"
		case labels != nil && labels["minikube.k8s.io/version"] != "":
			return "minikube"
		case strings.HasPrefix(providerID, "minikube://"):
			return "minikube"
		}
	}
	return "unknown"
}

// --- Phase 2: OpenBao install ---

// InstallOpenBao installs OpenBao via the helm binary, in dev mode
// (--install-secretstore-dev) or production mode (--install-secretstore).
func InstallOpenBao(ctx context.Context, cfg Config, deps Deps) (OpenBaoResult, error) {
	ns := cfg.OpenBaoNamespace
	if ns == "" {
		ns = DefaultOpenBaoNamespace
	}
	scheme := "http"
	if cfg.InstallSecretStore {
		scheme = "https"
	}
	result := OpenBaoResult{
		Namespace:      ns,
		ServiceAddress: fmt.Sprintf("%s://openbao.%s.svc:8200", scheme, ns),
	}

	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", openBaoRepoName, OpenBaoHelmRepoURL}); err != nil {
		return result, fmt.Errorf("add OpenBao helm repo: %w", err)
	}

	release, exists := findHelmRelease(ctx, deps, "openbao", ns)
	result.InstalledVersion = chartVersionFromRelease(release.Chart, "openbao")
	result.TargetVersion = cfg.OpenBaoChartVersion
	if result.TargetVersion == "" {
		result.TargetVersion = DefaultOpenBaoChartVersion
	}
	requiresTLSMigration := false
	if cfg.InstallSecretStore {
		deployed, verifiedTLS, err := openBaoDeploymentTLSState(ctx, deps, ns)
		if err != nil {
			return result, err
		}
		requiresTLSMigration = deployed && !verifiedTLS
		image, err := resolveOberthTLSBootstrapImage(ctx, cfg, deps)
		if err != nil {
			return result, err
		}
		caPEM, err := ensureOpenBaoTLS(ctx, cfg, deps, image)
		if err != nil {
			return result, fmt.Errorf("bootstrap verified OpenBao TLS: %w", err)
		}
		result.CACertPEM = caPEM
	}

	action := planHelmAction(exists, result.InstalledVersion, result.TargetVersion, release.Status, cfg.Upgrade)
	if action == actionSkip && requiresTLSMigration {
		// Listener TLS is a values-only reconciliation. Pin that reconciliation
		// to the installed chart so an older installer cannot downgrade a newer
		// OpenBao release while hardening its transport.
		if canonicalChartVersion(result.InstalledVersion) == "" {
			return result, errors.New("cannot safely reconcile TLS values for existing OpenBao release: " +
				"installed chart version could not be determined; rerun with --upgrade to force reconciliation")
		}
		cfg.OpenBaoChartVersion = result.InstalledVersion
		result.TargetVersion = result.InstalledVersion
		action = actionUpgrade
	}
	switch action {
	case actionSkip:
		result.AlreadyInstalled = true
		return result, nil
	case actionUpgrade:
		result.Upgraded = true
		if requiresTLSMigration && result.InstalledVersion == result.TargetVersion {
			_, _ = fmt.Fprintln(deps.Output, "Migrating OpenBao listener to verified TLS...")
		} else if result.InstalledVersion != "" && result.InstalledVersion != result.TargetVersion {
			_, _ = fmt.Fprintf(deps.Output, "OpenBao chart %s installed, %s available. Upgrading...\n",
				displayVersion(result.InstalledVersion), displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintln(deps.Output, "Upgrading OpenBao...")
		}
	case actionInstall:
		_, _ = fmt.Fprintln(deps.Output, "Installing OpenBao...")
	}

	if _, err := deps.RunHelm(ctx, OpenBaoHelmArgs(cfg)); err != nil {
		return result, fmt.Errorf("helm install OpenBao: %w", err)
	}
	if requiresTLSMigration {
		if err := restartOpenBaoAfterTLSMigration(ctx, cfg, deps, ns); err != nil {
			return result, err
		}
	}
	return result, nil
}

// OpenBaoHelmArgs returns the exact helm arguments for installing OpenBao.
//
// Dev mode (--install-secretstore-dev) runs the in-memory auto-unsealed dev
// server; helm --wait is safe because a dev server is Ready immediately.
//
// Production mode (--install-secretstore) runs the standalone server with
// persistent storage. It starts SEALED, and the chart's readiness probe (bao
// status) fails by design until `bao operator init` + unseal — so helm MUST
// NOT --wait here, or every fresh production install would time out.
// server.dev.enabled is pinned false because --reuse-values would otherwise
// carry a previous dev-mode install's dev flag through an --upgrade
// mode switch.
func OpenBaoHelmArgs(cfg Config) []string {
	ns := cfg.OpenBaoNamespace
	if ns == "" {
		ns = DefaultOpenBaoNamespace
	}
	chartVersion := cfg.OpenBaoChartVersion
	if chartVersion == "" {
		chartVersion = DefaultOpenBaoChartVersion
	}
	args := []string{
		"upgrade", "--install", "openbao", openBaoRepoName + "/openbao",
		"-n", ns, "--create-namespace",
		// Oberth fetches release secrets through the OpenBao API directly and
		// never uses the agent injector; the chart-default injector is an
		// unused privileged mutating webhook whose CLUSTER-scoped RBAC also
		// blocks any second OpenBao release on the same cluster. Off in both
		// modes.
		"--set", "injector.enabled=false",
	}
	if cfg.InstallSecretStoreDev {
		args = append(args,
			"--set", "global.tlsDisable=true",
			"--set", "server.dev.enabled=true",
		)
	} else {
		args = append(args,
			"--set", "global.tlsDisable=false",
			"--set", "server.dev.enabled=false",
			"--set", "server.standalone.enabled=true",
			"--set", "server.dataStorage.enabled=true",
			"--set", "server.statefulSet.securityContext.pod.runAsNonRoot=true",
			"--set", "server.statefulSet.securityContext.pod.runAsUser=100",
			"--set", "server.statefulSet.securityContext.pod.runAsGroup=1000",
			"--set", "server.statefulSet.securityContext.pod.fsGroup=1000",
			"--set", "server.statefulSet.securityContext.pod.fsGroupChangePolicy=OnRootMismatch",
			"--set", "server.statefulSet.securityContext.pod.seccompProfile.type=RuntimeDefault",
			"--set-string", "server.extraEnvironmentVars.BAO_CACERT="+openBaoTLSDirectory+"/ca.crt",
			"--set-string", "server.standalone.config="+openBaoStandaloneTLSConfig,
		)
	}
	args = append(args,
		"--version", chartVersion,
		// Idempotent re-runs and upgrades must not reset values a user set on
		// a previous install (OBERTH-RELEASE-024); no-op on first install.
		"--reuse-values",
	)
	if cfg.InstallSecretStoreDev {
		args = append(args, "--wait")
	}
	return args
}

// --- Phase 2b: Local Rekor stack (--install-rekor) ---

// InstallRekorStack installs the local Rekor transparency-log stack — MySQL,
// Trillian log server + signer, and the Rekor server — via the official
// sigstore/rekor umbrella chart, pinned to DefaultRekorChartVersion. The
// stack mirrors the docker-compose configuration validated during the
// 2026-08-08 test matrix: MySQL-backed Trillian AND a MySQL-backed search
// index (witness recovery depends on POST /api/v1/index/retrieve surviving
// pod restarts), plus a persistent file-based log signer.
func InstallRekorStack(ctx context.Context, cfg Config, deps Deps) (RekorResult, error) {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}
	result := RekorResult{
		Namespace: ns,
		// Chart service name "rekor-server" (release "rekor" + component
		// "server"), service port 80 → container 3000.
		ServiceAddress: fmt.Sprintf("http://rekor-server.%s.svc:80", ns),
	}

	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", sigstoreRepoName, SigstoreHelmRepoURL}); err != nil {
		return result, fmt.Errorf("add Sigstore helm repo: %w", err)
	}

	// Mint or reuse the persistent log signer key before the chart install:
	// the server deployment mounts the Secret at startup.
	publicPEM, err := EnsureRekorSignerKey(ctx, cfg, deps)
	if err != nil {
		return result, err
	}
	result.LogPublicKeyPEM = publicPEM

	release, exists := findHelmRelease(ctx, deps, "rekor", ns)
	result.InstalledVersion = chartVersionFromRelease(release.Chart, "rekor")
	result.TargetVersion = cfg.RekorChartVersion
	if result.TargetVersion == "" {
		result.TargetVersion = DefaultRekorChartVersion
	}

	switch planHelmAction(exists, result.InstalledVersion, result.TargetVersion, release.Status, cfg.Upgrade) {
	case actionSkip:
		result.AlreadyInstalled = true
		return result, nil
	case actionUpgrade:
		result.Upgraded = true
		if result.InstalledVersion != "" && result.InstalledVersion != result.TargetVersion {
			_, _ = fmt.Fprintf(deps.Output, "Rekor chart %s installed, %s available. Upgrading...\n",
				displayVersion(result.InstalledVersion), displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintln(deps.Output, "Upgrading Rekor...")
		}
	case actionInstall:
		_, _ = fmt.Fprintln(deps.Output, "Installing Rekor...")
	}

	// The search-index credential wiring is a structured env list; a values
	// file keeps it in one reviewable artifact instead of twenty --set
	// flags. Public configuration only — no secret material.
	valuesFile, err := os.CreateTemp("", "oberth-rekor-values-*.yaml")
	if err != nil {
		return result, fmt.Errorf("create Rekor values file: %w", err)
	}
	defer func() { _ = os.Remove(valuesFile.Name()) }()
	if _, err := valuesFile.WriteString(RekorHelmValuesYAML(cfg)); err != nil {
		_ = valuesFile.Close()
		return result, fmt.Errorf("write Rekor values file: %w", err)
	}
	if err := valuesFile.Close(); err != nil {
		return result, fmt.Errorf("close Rekor values file: %w", err)
	}

	if _, err := deps.RunHelm(ctx, RekorHelmArgs(cfg, valuesFile.Name())); err != nil {
		return result, fmt.Errorf("helm install Rekor: %w", err)
	}
	return result, nil
}

// RekorHelmArgs returns the exact helm arguments for installing the Rekor
// umbrella chart with the values file produced by RekorHelmValuesYAML.
func RekorHelmArgs(cfg Config, valuesPath string) []string {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}
	chartVersion := cfg.RekorChartVersion
	if chartVersion == "" {
		chartVersion = DefaultRekorChartVersion
	}
	return []string{
		"upgrade", "--install", "rekor", sigstoreRepoName + "/rekor",
		"-n", ns, "--create-namespace",
		"--version", chartVersion,
		"-f", valuesPath,
		// Idempotent re-runs and upgrades must not reset values a user set on
		// a previous install (OBERTH-RELEASE-024); the -f values above still
		// override reused ones. No-op on first install.
		"--reuse-values",
		"--wait",
	}
}

// RekorHelmValuesYAML renders the values for the sigstore/rekor umbrella
// chart (DefaultRekorChartVersion). Every deviation from chart defaults is
// deliberate:
//
//   - single namespace: the umbrella defaults scatter Trillian into a
//     separate trillian-system namespace; trillian.forceNamespace also feeds
//     the rekor-server --trillian_log_server.address argument, so it MUST
//     name the install namespace.
//   - no ingress: in-cluster consumers only; the chart-default nginx
//     ingress class renders a dead Ingress on k3s/kind.
//   - MySQL search index on the Trillian MySQL, redis disabled: the
//     chart-default redis has no persistence, and losing the search index
//     breaks witness-history recovery (POST /api/v1/index/retrieve) after a
//     restart. One durable store for both the log and its index.
//   - persistent file signer: the default memory signer regenerates the log
//     key on every pod restart, invalidating all previously published
//     witness entries.
//   - tuned resources: the untuned test-matrix MySQL ballooned to ~16GB
//     VSZ; requests total ≈1Gi for the whole stack, limits ≈2Gi.
func RekorHelmValuesYAML(cfg Config) string {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}
	return fmt.Sprintf(`# Generated by oberth install --install-rekor; not read back after install.
namespace:
  create: false
  name: %[1]s
trillian:
  enabled: true
  forceNamespace: %[1]s
  namespace:
    name: %[1]s
    create: false
  mysql:
    resources:
      requests:
        cpu: 100m
        memory: 512Mi
      limits:
        memory: 1Gi
  logServer:
    resources:
      requests:
        memory: 128Mi
      limits:
        memory: 256Mi
  logSigner:
    resources:
      requests:
        memory: 128Mi
      limits:
        memory: 256Mi
redis:
  enabled: false
mysql:
  enabled: false
server:
  ingress:
    enabled: false
  signer: %[2]s
  signerFileSecretOptions:
    secretName: %[3]s
    secretMountPath: %[4]s
    privateKeySecretKey: %[5]s
    secretMountSubPath: %[5]s
  gomemlimit: 400MiB
  resources:
    requests:
      memory: 128Mi
    limits:
      memory: 512Mi
  searchIndex:
    storageProvider: mysql
    mysql:
      envCredentials:
        - name: MYSQL_USER
          valueFrom:
            secretKeyRef:
              name: trillian-mysql
              key: mysql-user
        - name: MYSQL_PASSWORD
          valueFrom:
            secretKeyRef:
              name: trillian-mysql
              key: mysql-password
        - name: MYSQL_DATABASE
          valueFrom:
            secretKeyRef:
              name: trillian-mysql
              key: mysql-database
        - name: MYSQL_HOSTNAME
          value: trillian-mysql
        - name: MYSQL_PORT
          value: "3306"
`, ns, rekorSignerMountPath+"/"+rekorSignerSecretKey, rekorSignerSecretName, rekorSignerMountPath, rekorSignerSecretKey)
}

// EnsureRekorSignerKey mints — or reuses — the persistent Rekor log signing
// key and returns the log's PKIX PEM public key (the value Oberth will pin via
// auditAnchor.rekorPublicKey).
//
// An existing key is always reused, never rotated: the signing key is the
// local log's identity, and every witness entry ever published verifies
// against it. A malformed existing Secret is an error, not an overwrite.
func EnsureRekorSignerKey(ctx context.Context, cfg Config, deps Deps) (string, error) {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if _, err := deps.KubeClient.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create namespace %s: %w", ns, err)
	}

	existing, err := deps.KubeClient.CoreV1().Secrets(ns).Get(ctx, rekorSignerSecretName, metav1.GetOptions{})
	if err == nil {
		material, ok := existing.Data[rekorSignerSecretKey]
		if !ok || len(material) == 0 {
			return "", fmt.Errorf("secret %s/%s exists without key %q; refusing to overwrite a signer secret",
				ns, rekorSignerSecretName, rekorSignerSecretKey)
		}
		return rekorPublicKeyPEM(material)
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read signer secret: %w", err)
	}

	private, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate signer key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", fmt.Errorf("encode signer key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	// Best-effort zeroing of the local key copies once the Secret exists.
	// The authoritative copy intentionally lives in the cluster Secret —
	// the sigstore chart's file-signer mechanism — which is proportionate
	// for a local dev/evaluation transparency log.
	defer func() {
		for i := range der {
			der[i] = 0
		}
		for i := range privatePEM {
			privatePEM[i] = 0
		}
	}()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: rekorSignerSecretName, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{rekorSignerSecretKey: privatePEM},
	}
	if _, err := deps.KubeClient.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create signer secret: %w", err)
	}

	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return "", fmt.Errorf("encode signer public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})), nil
}

// rekorPublicKeyPEM derives the PKIX PEM public key from a PKCS#8 ECDSA
// private key PEM as stored in the signer Secret.
func rekorPublicKeyPEM(privatePEM []byte) (string, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil {
		return "", errors.New("signer secret does not contain PEM data")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse signer private key: %w", err)
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return "", errors.New("signer private key is not ECDSA")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return "", fmt.Errorf("encode signer public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})), nil
}

// --- Phase 4: Oberth install ---

// InstallOberth installs Oberth via the helm binary with its requested local
// secret store and Rekor audit witness wired.
func InstallOberth(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult, rekor RekorResult) (OberthResult, error) {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	result := OberthResult{Namespace: ns}
	if cfg.InstallSecretStore {
		if strings.TrimSpace(openbao.CACertPEM) == "" {
			return result, errors.New("installer-managed production OpenBao requires a public CA certificate before installing Oberth")
		}
		if !openbao.TrustedTransitVerified {
			return result, errors.New("installer-managed production OpenBao Transit provisioning was not positively verified; refusing to invoke Oberth Helm")
		}
	}

	release, exists := findHelmRelease(ctx, deps, "oberth", ns)
	result.InstalledVersion = chartVersionFromRelease(release.Chart, "oberth")
	result.TargetVersion = cfg.ChartVersion

	action := planHelmAction(exists, result.InstalledVersion, result.TargetVersion, release.Status, cfg.Upgrade)
	// A local chart has no published version to compare against, and its
	// content changes between two installs that would both report the same
	// version. Skipping would silently deploy the previous build.
	if strings.TrimSpace(cfg.ChartPath) != "" && action == actionSkip {
		action = actionUpgrade
		result.Upgraded = true
	}
	if action == actionSkip && (cfg.InstallRekor || cfg.wantsSecretStore()) {
		// Adding or hardening an optional service is a values change even when
		// the chart version is already current. Reconcile at the installed
		// version so an older installer cannot downgrade a newer deployment.
		if canonicalChartVersion(result.InstalledVersion) == "" {
			subject := "Rekor values"
			if cfg.wantsSecretStore() {
				subject = "secret store values"
			}
			return result, fmt.Errorf("cannot safely reconcile %s for existing Oberth release: "+
				"installed chart version could not be determined; rerun with --upgrade to force reconciliation", subject)
		}
		cfg.ChartVersion = result.InstalledVersion
		result.TargetVersion = result.InstalledVersion
		action = actionUpgrade
	}
	if action == actionSkip {
		result.AlreadyInstalled = true
		maybeHintNewerOberthChart(ctx, deps, result)
		return result, nil
	}
	switch {
	case strings.TrimSpace(cfg.ChartPath) != "":
		// A local chart resolves from disk, so there is no repository to add.
		// --image was assumed to name an image the node already has, which
		// cannot hold when this installer is what created the node: put it
		// there from the local daemon if the node lacks it.
		if err := ensureLocalImageInNode(ctx, deps, strings.TrimSpace(cfg.ImageRef)); err != nil {
			return result, err
		}
	case deps.KindClusterName != "":
		if err := prepareKindImagesForCluster(ctx, cfg, deps, deps.KindClusterName); err != nil {
			return result, fmt.Errorf("prepare kind images: %w", err)
		}
	default:
		if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", oberthRepoName, OberthHelmRepoURL}); err != nil {
			return result, fmt.Errorf("add Oberth helm repo: %w", err)
		}
	}

	switch action {
	case actionUpgrade:
		result.Upgraded = true
		if result.InstalledVersion != "" && result.InstalledVersion != result.TargetVersion {
			_, _ = fmt.Fprintf(deps.Output, "Oberth %s installed, %s available. Upgrading...\n",
				displayVersion(result.InstalledVersion), displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintln(deps.Output, "Upgrading Oberth...")
		}
	case actionInstall:
		if result.TargetVersion != "" {
			_, _ = fmt.Fprintf(deps.Output, "Installing Oberth %s...\n", displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintln(deps.Output, "Installing Oberth...")
		}
	}

	if _, err := deps.RunHelm(ctx, OberthHelmArgs(cfg, openbao, rekor)); err != nil {
		return result, fmt.Errorf("helm install Oberth: %w", err)
	}
	return result, nil
}

// maybeHintNewerOberthChart is a best-effort advisory for the skip path:
// refresh the Oberth chart repo index and, when it lists a chart strictly
// newer than the one this binary targets, tell the user the path to it (a
// newer binary — the install always deploys its own paired chart version).
// It never changes the skip decision and never fails the install.
func maybeHintNewerOberthChart(ctx context.Context, deps Deps, result OberthResult) {
	target := canonicalChartVersion(result.TargetVersion)
	if target == "" {
		return
	}
	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", oberthRepoName, OberthHelmRepoURL}); err != nil {
		return
	}
	latest := latestChartVersion(ctx, deps, oberthRepoName+"/oberth")
	canonical := canonicalChartVersion(latest)
	if canonical == "" || semver.Compare(canonical, target) <= 0 {
		return
	}
	_, _ = fmt.Fprintf(deps.Output,
		"Note: chart %s is available; upgrade the oberth binary (e.g. brew upgrade oberth) to install it.\n",
		displayVersion(latest))
}

// OberthHelmArgs returns the exact helm arguments for installing Oberth.
// Optional service values are emitted only when their matching installer flag
// was requested; both chart features remain disabled by default.
func OberthHelmArgs(cfg Config, openbao OpenBaoResult, rekor RekorResult) []string {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	chart := oberthRepoName + "/oberth"
	if local := strings.TrimSpace(cfg.ChartPath); local != "" {
		chart = local
	}
	args := []string{
		"upgrade", "--install", "oberth", chart,
		"-n", ns, "--create-namespace",
	}
	// Before every --set below, so the installer's own values take precedence,
	// and in the order given, because helm resolves overlapping files by
	// argument order and reordering them would invert an override.
	for _, file := range cfg.ValuesFiles {
		args = append(args, "-f", file)
	}
	if image := strings.TrimSpace(cfg.ImageRef); image != "" {
		args = append(args, "--set-string", "image.ref="+image)
	}
	// Indexed --set-string rather than --set name={a,b}: brace list syntax
	// splits on commas inside the value, and a name is not worth losing to
	// quoting rules.
	for index, name := range cfg.TLSExtraDNSNames {
		args = append(args, "--set-string", fmt.Sprintf("tls.extraDNSNames[%d]=%s", index, name))
	}
	for index, address := range cfg.TLSExtraIPs {
		args = append(args, "--set-string", fmt.Sprintf("tls.extraIPs[%d]=%s", index, address))
	}
	args = append(args, argoOberthHelmValues(cfg, openbao)...)
	if cfg.wantsSecretStore() {
		args = append(args,
			"--set", "secretstore.enabled=true",
			"--set", "secretstore.address="+openbao.ServiceAddress,
			"--set", "secretstore.role="+defaultRole,
		)
		if cfg.InstallSecretStoreDev {
			args = append(args,
				"--set-string", "secretstore.caCert=",
				"--set", "secretstore.insecureHTTPForDev=true",
				"--set", "secretstore.transit.enabled=false",
			)
		} else {
			transitEnabled := "false"
			if openbao.TrustedTransitVerified {
				transitEnabled = "true"
			}
			args = append(args,
				"--set-string", "secretstore.caCert="+openbao.CACertPEM,
				"--set", "secretstore.insecureHTTPForDev=false",
				"--set", "secretstore.transit.enabled="+transitEnabled,
			)
			if openbao.TrustedTransitVerified {
				// --reuse-values discards defaults newly added by a newer chart.
				// Pin the exact identity that was positively verified so upgrades
				// from pre-Transit releases cannot enable an incomplete config.
				args = append(args,
					"--set", "secretstore.transit.mount="+defaultTransitMount,
					"--set", "secretstore.transit.key="+defaultTransitKey,
				)
			}
		}
	}
	if cfg.InstallRekor {
		args = append(args,
			"--set", "auditAnchor.rekorURL="+rekor.ServiceAddress,
			// rekor-server has no native TLS listener. This mirrors the
			// secretstore.insecureHTTPForDev escape valve above: plain HTTP is
			// enabled explicitly and only for the installer-managed in-cluster
			// endpoint, while the pinned log key protects entry authenticity.
			"--set", "auditAnchor.rekorInsecureHTTP=true",
			"--set-string", "auditAnchor.rekorPublicKey="+rekor.LogPublicKeyPEM,
		)
		if cfg.AcceptWitnessGenesis != "" {
			args = append(args, "--set-string", "auditAnchor.acceptWitnessGenesis="+cfg.AcceptWitnessGenesis)
		} else {
			// One-shot hygiene: --reuse-values would otherwise carry a previous
			// retrofit's acknowledgment into every future upgrade forever.
			args = append(args, "--set-string", "auditAnchor.acceptWitnessGenesis=")
		}
	}
	// Pipeline egress NetworkPolicy: pin explicitly on every install/upgrade.
	// --reuse-values discards new chart defaults, so this must be explicit.
	if cfg.NetworkPolicy == "true" || cfg.NetworkPolicy == "false" {
		args = append(args, "--set", "networkPolicy.enabled="+cfg.NetworkPolicy)
	}
	// --version selects a published chart; a local path already IS the version.
	if cfg.ChartVersion != "" && strings.TrimSpace(cfg.ChartPath) == "" {
		args = append(args, "--version", cfg.ChartVersion)
	}
	// Idempotent re-runs and upgrades must not reset values a user set on a
	// previous install (OBERTH-RELEASE-024) — in particular an existing
	// secretstore configuration survives a re-run without
	// --install-secretstore. The --set flags above still override reused
	// values. No-op on first install.
	args = append(args, "--reuse-values")
	return args
}

// --- Helm helpers ---

type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
	// Chart is helm's "<chart-name>-<chart-version>" field, e.g.
	// "oberth-0.10.58".
	Chart string `json:"chart"`
}

// findHelmRelease looks the named release up via `helm list -o json`. Helm
// failures and unparseable output are treated as release-absent: the callers
// recover through `helm upgrade --install`, which is correct for both a
// genuinely missing release and a namespace helm cannot list yet.
func findHelmRelease(ctx context.Context, deps Deps, name, namespace string) (helmRelease, bool) {
	out, err := deps.RunHelm(ctx, []string{"list", "-n", namespace, "-o", "json"})
	if err != nil {
		return helmRelease{}, false
	}
	var releases []helmRelease
	if err := json.Unmarshal(out, &releases); err != nil {
		return helmRelease{}, false
	}
	for _, r := range releases {
		if r.Name == name {
			return r, true
		}
	}
	return helmRelease{}, false
}

// chartVersionFromRelease extracts the chart version from a helm release
// chart field ("oberth-0.10.58" → "0.10.58"). Returns "" when the field does
// not carry the expected "<chartName>-" prefix.
func chartVersionFromRelease(chartField, chartName string) string {
	rest, ok := strings.CutPrefix(chartField, chartName+"-")
	if !ok || rest == "" {
		return ""
	}
	return rest
}

// canonicalChartVersion normalizes a chart version to the "v"-prefixed form
// required by golang.org/x/mod/semver ("" when the input is not a valid
// semantic version).
func canonicalChartVersion(raw string) string {
	if raw == "" {
		return ""
	}
	v := "v" + strings.TrimPrefix(raw, "v")
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// displayVersion renders a chart version for user-facing messages ("0.10.58"
// and "v0.10.58" both display as "v0.10.58").
func displayVersion(raw string) string {
	return "v" + strings.TrimPrefix(raw, "v")
}

// helmAction is the idempotent-install decision for one helm release.
type helmAction int

const (
	// actionInstall: the release is absent — install it.
	actionInstall helmAction = iota
	// actionUpgrade: the release exists and a strictly newer target chart is
	// requested (or --upgrade forces reconciliation) — run helm upgrade.
	actionUpgrade
	// actionSkip: the release exists and is already at (or beyond) the target
	// version, or the versions cannot be compared — leave it untouched.
	actionSkip
)

// planHelmAction decides between install, upgrade, and skip. It is never
// destructive: an existing release is only touched when the target chart
// version is strictly newer than the installed one, or when force
// (--upgrade) explicitly requests reconciliation. Unknown or unparseable
// versions on either side skip, preserving whatever is running.
//
// A non-deployed release (e.g. status "failed" or "pending-install" from a
// previous timed-out or crashed install) is always reconciled: silently
// skipping a broken release leaves the component down with no user-visible
// signal, and the operator must discover the --upgrade escape hatch.
func planHelmAction(exists bool, installed, target, status string, force bool) helmAction {
	if !exists {
		return actionInstall
	}
	if status != "" && status != "deployed" {
		return actionUpgrade
	}
	if force {
		return actionUpgrade
	}
	i, t := canonicalChartVersion(installed), canonicalChartVersion(target)
	if i == "" || t == "" {
		return actionSkip
	}
	if semver.Compare(t, i) > 0 {
		return actionUpgrade
	}
	return actionSkip
}

type helmSearchEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// latestChartVersion returns the newest chart version the local helm repo
// cache knows for chartRef ("" on any failure — the lookup is best-effort
// and never blocks an install decision).
func latestChartVersion(ctx context.Context, deps Deps, chartRef string) string {
	out, err := deps.RunHelm(ctx, []string{"search", "repo", chartRef, "-o", "json"})
	if err != nil {
		return ""
	}
	var entries []helmSearchEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name == chartRef {
			return entry.Version
		}
	}
	return ""
}

// --- Phase 5: Wait for Ready ---

// WaitForReady watches the Oberth pod until it becomes Ready or the timeout expires.
func WaitForReady(ctx context.Context, cfg Config, deps Deps) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	return waitForPodReady(ctx, deps, ns, "app.kubernetes.io/instance=oberth", cfg.Timeout)
}

func waitForPodReady(ctx context.Context, deps Deps, namespace, labelSelector string, timeout time.Duration) error {
	return waitForPodCondition(ctx, deps, namespace, labelSelector, timeout, isPodReady)
}

// waitForPodRunning waits for a pod that can accept kubectl exec — Running
// phase is enough; readiness is NOT required (a sealed OpenBao and a fresh
// Oberth without an upstream both stay NotReady by design).
func waitForPodRunning(ctx context.Context, deps Deps, namespace, labelSelector string, timeout time.Duration) error {
	return waitForPodCondition(ctx, deps, namespace, labelSelector, timeout, isPodRunning)
}

func waitForPodCondition(ctx context.Context, deps Deps, namespace, labelSelector string, timeout time.Duration, condition func(*corev1.Pod) bool) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Anchor the Watch to the List's resourceVersion so transitions between
	// the List and the Watch establishment are not missed. Without this, a
	// pod that becomes ready in that gap is never observed and the install
	// falsely times out.
	pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(deadline, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err == nil {
		for i := range pods.Items {
			if condition(&pods.Items[i]) {
				return nil
			}
		}
	}

	watchOpts := metav1.ListOptions{LabelSelector: labelSelector}
	if pods != nil {
		watchOpts.ResourceVersion = pods.ResourceVersion
	}
	watcher, err := deps.KubeClient.CoreV1().Pods(namespace).Watch(deadline, watchOpts)
	if err != nil {
		return fmt.Errorf("watch pods in %s: %w", namespace, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-deadline.Done():
			return printTimeoutDiagnostics(deps, namespace, labelSelector, timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Watch closed (expired, network error). Re-list and re-watch
				// rather than timing out, so a closed watch does not lose a
				// transition that happened after the list.
				pods, err = deps.KubeClient.CoreV1().Pods(namespace).List(deadline, metav1.ListOptions{
					LabelSelector: labelSelector,
				})
				if err == nil {
					for i := range pods.Items {
						if condition(&pods.Items[i]) {
							return nil
						}
					}
					watchOpts.ResourceVersion = pods.ResourceVersion
				}
				watcher, err = deps.KubeClient.CoreV1().Pods(namespace).Watch(deadline, watchOpts)
				if err != nil {
					return printTimeoutDiagnostics(deps, namespace, labelSelector, timeout)
				}
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if condition(pod) {
				return nil
			}
		}
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isPodRunning accepts a Running pod; a Ready pod is by definition running
// even when a test fixture omits the phase.
func isPodRunning(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning || isPodReady(pod)
}

func printTimeoutDiagnostics(deps Deps, namespace, labelSelector string, timeout time.Duration) error {
	_, _ = fmt.Fprintf(deps.Output, "\nTimeout (%s) waiting for pod to become ready in namespace %s\n", timeout, namespace)

	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(freshCtx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err == nil && len(pods.Items) > 0 {
		for _, pod := range pods.Items {
			_, _ = fmt.Fprintf(deps.Output, "  Pod %s: phase=%s\n", pod.Name, pod.Status.Phase)
			for _, c := range pod.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					msg := string(c.Status)
					if c.Message != "" {
						msg += " — " + c.Message
					}
					_, _ = fmt.Fprintf(deps.Output, "    %s: %s\n", c.Type, msg)
				}
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					reason := cs.State.Waiting.Reason
					if cs.State.Waiting.Message != "" {
						reason += " — " + cs.State.Waiting.Message
					}
					_, _ = fmt.Fprintf(deps.Output, "    Container %s waiting: %s\n", cs.Name, reason)
				}
			}
		}
	} else if err == nil {
		_, _ = fmt.Fprintf(deps.Output, "  No pods found with label %s in namespace %s\n", labelSelector, namespace)
	}

	_, _ = fmt.Fprintf(deps.Output, "\nCommon causes:\n")
	_, _ = fmt.Fprintf(deps.Output, "  - Image pull: verify the image ref and pull policy\n")
	_, _ = fmt.Fprintf(deps.Output, "  - PVC pending: check storage class availability\n")
	_, _ = fmt.Fprintf(deps.Output, "  - Secretstore auth not ready: re-run oberth install\n")
	_, _ = fmt.Fprintf(deps.Output, "\nDiagnostic commands:\n")
	_, _ = fmt.Fprintf(deps.Output, "  kubectl get pods -n %s\n", namespace)
	_, _ = fmt.Fprintf(deps.Output, "  kubectl describe pods -n %s -l %s\n", namespace, labelSelector)
	_, _ = fmt.Fprintf(deps.Output, "  kubectl logs -n %s -l %s\n", namespace, labelSelector)

	return fmt.Errorf("timeout (%s) waiting for pod ready in namespace %s", timeout, namespace)
}

// --- Phase 6: Next steps ---

// PrintNextSteps prints post-install guidance matching charts/oberth/templates/NOTES.txt.
func PrintNextSteps(cfg Config, w io.Writer) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	installed := "Oberth is installed."
	if cfg.InstallSecretStore {
		installed = "Oberth is installed and connected to OpenBao."
	}
	_, err := fmt.Fprintf(w, `
%[2]s

Next steps:

  1. Register the upstream (interactive — generates and persists the deploy key):

       kubectl exec -it -n %[1]s deploy/oberth -- \
         oberth upstream add <name> ssh://git@<forge>/<org>

  2. Add your first uplink (prints its bearer token and TLS fingerprint once):

       kubectl exec -i -n %[1]s deploy/oberth -- \
         oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub

  3. Clone an existing repository through Oberth:

       git clone ssh://git@localhost:30022/<org>/<repo>.git

  4. Generate a pipeline and push:

       cd <repo>
       oberth init
       git add .oberth/ && git commit -m "add oberth pipeline"
       git push origin main

`, ns, installed)
	return err
}

func printDryRunPlan(cfg Config, w io.Writer, cluster ClusterInfo) error {
	return printDryRunPlanWithKind(cfg, w, cluster, nil)
}

func printDryRunPlanWithKind(cfg Config, w io.Writer, cluster ClusterInfo, kindConfig []byte) error {
	_, _ = fmt.Fprintf(w, "\nDry-run plan (no cluster changes will be made):\n\n")
	step := 1

	if len(kindConfig) > 0 {
		_, _ = fmt.Fprintf(w, "macOS detected: use kind cluster %q (context %q), not k3s or k0s.\n\n", KindClusterName, KindContextName)
		_, _ = fmt.Fprintf(w, "  %d. Check prerequisites\n", step)
		_, _ = fmt.Fprintln(w, "     Docker Desktop or OrbStack is installed and running (docker info)")
		_, _ = fmt.Fprintln(w, "     kind is installed (brew install kind)")
		_, _ = fmt.Fprintln(w, "     helm is installed (brew install helm)")
		step++
		_, _ = fmt.Fprintf(w, "\n  %d. Create kind cluster %q with this config\n\n", step, KindClusterName)
		_, _ = fmt.Fprintf(w, "     cat <<'OBERTH_KIND_CONFIG' | kind create cluster --name %s --config=-\n", KindClusterName)
		_, _ = fmt.Fprintln(w, strings.TrimSpace(string(kindConfig)))
		_, _ = fmt.Fprintln(w, "OBERTH_KIND_CONFIG")
		step++
	}

	oberthRepoAdded := false
	if cluster.Engine == "kind" {
		clusterName := KindClusterName
		if parsed, ok := kindNameFromContext(cluster.Context); ok {
			clusterName = parsed
		}
		showArgs := oberthChartValuesArgs(cfg)
		_, _ = fmt.Fprintf(w, "\n  %d. Resolve, pull, and load the chart images into kind\n", step)
		_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", oberthRepoName, OberthHelmRepoURL)
		oberthRepoAdded = true
		_, _ = fmt.Fprintf(w, "     helm %s\n", strings.Join(showArgs, " "))
		_, _ = fmt.Fprintln(w, "     # Set OBERTH_IMAGE_REF from image.ref above. Job pods pull their own")
		_, _ = fmt.Fprintln(w, "     # repository-declared golang images at run time; only the server image loads here.")
		_, _ = fmt.Fprintln(w, "     # The chart pins multi-arch index digests; docker cannot save a partially")
		_, _ = fmt.Fprintln(w, "     # pulled index, so for each ref: resolve this platform's child manifest,")
		_, _ = fmt.Fprintln(w, "     # pull and load it under a local tag, then alias the pinned digest on the")
		_, _ = fmt.Fprintln(w, "     # kind node so digest-referencing pods resolve the loaded content.")
		_, _ = fmt.Fprintln(w, "     docker manifest inspect \"$OBERTH_IMAGE_REF\"  # copy .manifests[] digest for linux/<arch> into OBERTH_IMAGE_CHILD_REF")
		_, _ = fmt.Fprintln(w, "     docker pull \"$OBERTH_IMAGE_CHILD_REF\"")
		_, _ = fmt.Fprintln(w, "     docker tag \"$OBERTH_IMAGE_CHILD_REF\" \"$OBERTH_IMAGE_LOCAL_TAG\"")
		_, _ = fmt.Fprintf(w, "     kind load docker-image --name %s \"$OBERTH_IMAGE_LOCAL_TAG\"\n", clusterName)
		_, _ = fmt.Fprintf(w, "     docker exec %s-control-plane ctr --namespace=k8s.io images tag --force \"$OBERTH_IMAGE_LOCAL_TAG\" \"$OBERTH_IMAGE_REF\"\n", clusterName)
		step++
	}

	openbaoAddr := ""
	var rekor RekorResult
	if cfg.wantsSecretStore() {
		ns := cfg.OpenBaoNamespace
		if ns == "" {
			ns = DefaultOpenBaoNamespace
		}
		if cfg.InstallSecretStore {
			_, _ = fmt.Fprintf(w, "  %d. Stage verified OpenBao TLS before any listener exists\n", step)
			if !oberthRepoAdded {
				_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", oberthRepoName, OberthHelmRepoURL)
				oberthRepoAdded = true
			}
			_, _ = fmt.Fprintf(w, "     helm %s\n", strings.Join(oberthChartValuesArgs(cfg), " "))
			_, _ = fmt.Fprintf(w, "     Pre-create or adopt PVC %s/%s (10Gi, ReadWriteOnce)\n", ns, openBaoTLSDataClaimName)
			_, _ = fmt.Fprintln(w, "     Run the credential-free, digest-pinned TLS bootstrap Pod against that PVC")
			_, _ = fmt.Fprintln(w, "     Keep private CA/server keys on the PVC; capture only the public CA certificate")
			_, _ = fmt.Fprintln(w)
			step++
		}
		if cfg.InstallSecretStoreDev {
			_, _ = fmt.Fprintf(w, "  %d. Install OpenBao (dev mode: in-memory, auto-unsealed) into namespace %q\n", step, ns)
		} else {
			_, _ = fmt.Fprintf(w, "  %d. Install OpenBao (production mode: standalone server + persistent storage) into namespace %q\n", step, ns)
		}
		_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", openBaoRepoName, OpenBaoHelmRepoURL)
		args := OpenBaoHelmArgs(cfg)
		_, _ = fmt.Fprintf(w, "     helm %s\n\n", strings.Join(args, " "))
		step++

		if cfg.InstallSecretStore {
			_, _ = fmt.Fprintf(w, "  %d. Initialize and unseal OpenBao (production mode; via kubectl exec, no port-forward)\n", step)
			_, _ = fmt.Fprintf(w, "     kubectl exec -n %s <openbao-pod> -- bao operator init -key-shares=1 -key-threshold=1 -format=json\n", ns)
			_, _ = fmt.Fprintln(w, "     Print the root token and unseal key ONCE — save them securely")
			_, _ = fmt.Fprintln(w, "     kubectl exec: bao operator unseal (key passed via stdin, never argv)")
			_, _ = fmt.Fprintln(w, "     Wait for the unsealed pod to become Ready")
			_, _ = fmt.Fprintln(w)
			step++
		}

		_, _ = fmt.Fprintf(w, "  %d. Configure secret store (kubernetes auth, policy, role; via kubectl exec bao commands)\n", step)
		_, _ = fmt.Fprintf(w, "     Enable auth method: kubernetes at %s/\n", defaultAuthMount)
		_, _ = fmt.Fprintln(w, "     Write auth config: kubernetes_host=https://kubernetes.default.svc")
		_, _ = fmt.Fprintf(w, "     Enable KV v2 mount: %s/\n", defaultKVPrefix)
		if cfg.InstallSecretStore {
			_, _ = fmt.Fprintf(w, "     Enable Transit mount: %s/ and create non-exportable key %s\n", defaultTransitMount, defaultTransitKey)
			_, _ = fmt.Fprintf(w, "     Write policy: %s (read %s/data/*; update only exact Transit encrypt/decrypt paths)\n", defaultPolicy, defaultKVPrefix)
		} else {
			_, _ = fmt.Fprintf(w, "     Write policy: %s (read-only on %s/data/*; Transit disabled)\n", defaultPolicy, defaultKVPrefix)
		}
		oberthNS := cfg.Namespace
		if oberthNS == "" {
			oberthNS = DefaultNamespace
		}
		_, _ = fmt.Fprintf(w, "     Write role: %s bound to %s/%s ServiceAccount\n\n", defaultRole, oberthNS, defaultServiceAccount)
		step++

		scheme := "http"
		if cfg.InstallSecretStore {
			scheme = "https"
		}
		openbaoAddr = fmt.Sprintf("%s://openbao.%s.svc:8200", scheme, ns)
	}

	if cfg.InstallRekor {
		rekorNS := cfg.RekorNamespace
		if rekorNS == "" {
			rekorNS = DefaultRekorNamespace
		}
		_, _ = fmt.Fprintf(w, "  %d. Install the local Rekor stack (MySQL + Trillian + Rekor server) into namespace %q\n", step, rekorNS)
		_, _ = fmt.Fprintf(w, "     Mint persistent log signer key: Secret %s/%s (reused if already present)\n", rekorNS, rekorSignerSecretName)
		rekorArgs := RekorHelmArgs(cfg, "<generated-values.yaml>")
		_, _ = fmt.Fprintf(w, "     helm %s\n", strings.Join(rekorArgs, " "))
		_, _ = fmt.Fprintf(w, "     Requires ~1Gi additional RAM (requests; ~2Gi limits) — recommended for 32GB+ machines\n")
		_, _ = fmt.Fprintln(w)
		rekor = RekorResult{
			ServiceAddress:  fmt.Sprintf("http://rekor-server.%s.svc:80", rekorNS),
			LogPublicKeyPEM: "<generated-log-public-key-pem>",
		}
		step++
	}

	argoNS := cfg.ArgoNamespace
	if argoNS == "" {
		argoNS = DefaultArgoNamespace
	}
	if cfg.SkipArgo {
		_, _ = fmt.Fprintf(w, "  %d. Skip installing Argo Workflows (--skip-argo-workflows)\n", step)
		_, _ = fmt.Fprintf(w, "     Verify an existing ready workflow controller in namespace %q\n", argoNS)
		_, _ = fmt.Fprintln(w, "     Oberth executes every pipeline as an Argo Workflow; without a controller")
		_, _ = fmt.Fprintln(w, "     watching that namespace a push is accepted and then never executes")
		_, _ = fmt.Fprintln(w)
	} else {
		_, _ = fmt.Fprintf(w, "  %d. Install Argo Workflows (execution engine) into namespace %q\n", step, argoNS)
		_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", argoRepoName, ArgoHelmRepoURL)
		_, _ = fmt.Fprintf(w, "     helm %s\n", strings.Join(ArgoHelmArgs(cfg), " "))
		_, _ = fmt.Fprintln(w, "     Namespace-scoped controller only: no Argo Server, no cluster-scoped RBAC")
		_, _ = fmt.Fprintln(w)
	}
	step++

	oberthNS := cfg.Namespace
	if oberthNS == "" {
		oberthNS = DefaultNamespace
	}
	_, _ = fmt.Fprintf(w, "  %d. Install Oberth into namespace %q\n", step, oberthNS)
	if !oberthRepoAdded {
		_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", oberthRepoName, OberthHelmRepoURL)
	}
	openbao := OpenBaoResult{ServiceAddress: openbaoAddr}
	if cfg.InstallSecretStore {
		openbao.CACertPEM = "<installer-managed-public-ca-pem>"
		// The dry-run orders this Helm step after the exact provisioning step
		// above; show the value that is emitted only if that step succeeds.
		openbao.TrustedTransitVerified = true
	}
	oberthArgs := OberthHelmArgs(cfg, openbao, rekor)
	_, _ = fmt.Fprintf(w, "     helm %s\n\n", strings.Join(oberthArgs, " "))
	step++

	_, _ = fmt.Fprintf(w, "  %d. Wait for the Oberth pod to start (timeout: %s), then complete first-run onboarding\n", step, cfg.Timeout)
	_, _ = fmt.Fprintln(w, "     A fresh deployment stays NotReady until its first upstream is registered;")
	_, _ = fmt.Fprintln(w, "     an interactive terminal is walked through upstream + uplink setup, otherwise")
	_, _ = fmt.Fprintln(w, "     the manual kubectl exec steps are printed.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "No cluster changes were made (--dry-run).")
	return nil
}

// --- Default implementations ---

// DefaultRunHelm executes the helm binary with the given arguments.
// Stdout and stderr are captured separately: on success only stdout is
// returned (clean data for JSON/YAML callers), on failure both are included
// for diagnostics. This prevents helm stderr warnings (e.g. kubeconfig
// permission notices on macOS) from corrupting parseable output.
func DefaultRunHelm(ctx context.Context, args []string) ([]byte, error) {
	// #nosec G204 -- the command is the literal helm binary; every argument is
	// constructed by this package from validated flags (no shell, no
	// caller-controlled command word).
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		summary := strings.Join(args, " ")
		if len(args) > 3 {
			summary = strings.Join(args[:3], " ") + "..."
		}
		combined := stdout.String() + stderr.String()
		return []byte(combined), fmt.Errorf("helm %s: %w\n%s", summary, err, combined)
	}
	return stdout.Bytes(), nil
}

// k3sKubeconfigPath is the default kubeconfig location written by k3s.
const k3sKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// LoadKubeConfigForContext loads Kubernetes client configuration from standard
// rules and, when set, selects the exact requested context. When the standard
// load fails and no explicit context or KUBECONFIG is set, it falls back to the
// k3s kubeconfig at /etc/rancher/k3s/k3s.yaml.
func LoadKubeConfigForContext(contextName string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	return loadKubeConfigForContext(contextName, k3sKubeconfigPath, output)
}

// loadKubeConfigForContext is the testable core: the k3s fallback path is
// injected so tests can point it at a temp file or an unreadable path, and
// the standard loading rules are injected so tests can simulate an empty
// default search path (clientcmd.RecommendedHomeFile is a package-level
// variable initialized at program start from the original $HOME, which
// t.Setenv cannot override).
func loadKubeConfigForContext(contextName, k3sFallbackPath string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	return loadKubeConfigWithRules(clientcmd.NewDefaultClientConfigLoadingRules(), contextName, k3sFallbackPath, output)
}

func loadKubeConfigWithRules(rules *clientcmd.ClientConfigLoadingRules, contextName, k3sFallbackPath string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	client, restConfig, selectedContext, err := loadFromRules(rules, contextName)
	if err == nil {
		return client, restConfig, selectedContext, nil
	}
	// Standard kubeconfig load failed. Fall back to the k3s path only when
	// no explicit context was requested and $KUBECONFIG is not set — an
	// explicit KUBECONFIG, even pointing at a missing file, is the user's
	// stated intent and must not be silently overridden.
	if contextName != "" || os.Getenv("KUBECONFIG") != "" || k3sFallbackPath == "" {
		return nil, nil, "", err
	}
	// Probe readability before committing to the fallback. k3s defaults
	// k3s.yaml to 0600 root, so a non-root user gets EACCES; returning
	// that as a new error would hide the original "no configuration" message.
	probe, openErr := os.Open(k3sFallbackPath) // #nosec G304 -- fixed or test-injected path
	if openErr != nil {
		return nil, nil, "", err
	}
	_ = probe.Close()
	// Load k3s.yaml as the sole source via ExplicitPath so that its
	// "default" context cannot hijack selection in a merged config.
	k3sRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: k3sFallbackPath}
	client, restConfig, selectedContext, k3sErr := loadFromRules(k3sRules, "")
	if k3sErr != nil {
		return nil, nil, "", k3sErr
	}
	if output != nil {
		_, _ = fmt.Fprintf(output, "Using k3s kubeconfig at %s\n", k3sFallbackPath)
	}
	return client, restConfig, selectedContext, nil
}

func loadFromRules(rules *clientcmd.ClientConfigLoadingRules, contextName string) (kubernetes.Interface, *rest.Config, string, error) {
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	rawConfig, err := config.RawConfig()
	if err != nil {
		return nil, nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, nil, "", fmt.Errorf("build REST config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create Kubernetes client: %w", err)
	}
	selectedContext := rawConfig.CurrentContext
	if contextName != "" {
		selectedContext = contextName
	}
	return client, restConfig, selectedContext, nil
}
