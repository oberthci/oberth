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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/kubeconfig"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oberthci/oberth/internal/podpriority"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const (
	// DefaultOpenBaoNamespace is the namespace for the dev OpenBao install.
	DefaultOpenBaoNamespace = "openbao"
	// DefaultNamespace is the namespace for Oberth.
	DefaultNamespace = "oberth"
	// DefaultTimeout is the default wait-for-ready deadline.
	// DefaultTimeout bounds each readiness wait. A first install pulls the
	// OpenBao, Argo and Oberth images (and Rekor's stack) onto an empty node,
	// which five minutes did not reliably cover.
	DefaultTimeout = 15 * time.Minute
	// DefaultOpenBaoChartVersion is the pinned OpenBao chart version.
	// Bump deliberately and re-verify the dev-mode install flow; never float
	// to latest.
	DefaultOpenBaoChartVersion = "0.28.6"

	// OpenBaoHelmRepoURL is the canonical OpenBao Helm chart repository.
	OpenBaoHelmRepoURL = "https://openbao.github.io/openbao-helm"
	// OberthHelmRepoURL is the canonical Oberth Helm chart repository — the
	// Oberth-specific public chart index.
	OberthHelmRepoURL = "https://charts.oberth.ci"

	openBaoRepoName = "openbao"
	oberthRepoName  = "oberth-charts"

	// SigstoreHelmRepoURL is the canonical Sigstore Helm chart repository,
	// serving the rekor umbrella chart (bundled trillian + createtree).
	SigstoreHelmRepoURL = "https://sigstore.github.io/helm-charts"
	// DefaultRekorNamespace is the namespace for the local Rekor stack.
	DefaultRekorNamespace = "rekor"
	// DefaultRekorChartVersion is the pinned sigstore/rekor umbrella chart
	// adaptation (appVersion v1.5.3; bundles trillian chart 0.3.16). Bump
	// deliberately and re-verify the local witness flow against
	// RekorHelmValuesYAML; never float to latest.
	DefaultRekorChartVersion = "1.8.3-oberth.1"

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
	ReissueServerCA        bool
	KubeconfigPath         string
	ContextName            string
	UpstreamPrivateKeyPath string

	Dev        bool
	Production bool
	DryRun     bool
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
	// true, Run keeps an existing store or proposes production mode
	// interactively. A fresh non-interactive install requires an explicit flag.
	SecretStoreUndecided bool
	// InstallRekor additionally installs a local Rekor transparency-log
	// stack — MySQL + Trillian log server/signer + Rekor server — as the
	// audit witness (--install-rekor). Independent of the secret store.
	InstallRekor    bool
	rekorPublicKey  string
	rekorBaoAddress string
	rekorBaoCA      string
	// SkipArgo opts out of installing the Argo Workflows controller
	// (--skip-argo-workflows), for a cluster that already runs one watching
	// ArgoNamespace. It does not make pipelines execute any other way: Argo is
	// the execution engine, so the install still binds Oberth to that
	// namespace and still verifies a controller is really there.
	SkipArgo              bool
	OpenBaoNamespace      string
	Namespace             string
	RekorNamespace        string
	ArgoNamespace         string
	ChartVersion          string
	OpenBaoChartVersion   string
	RekorChartVersion     string
	ArgoChartVersion      string
	ArgoControllerProfile string
	// ArgoValuesReviewed acknowledges stored Argo values outside the nonroot
	// profile's allowlist: the SHA-256 of `helm get values argo-workflows -o
	// json` as reviewed (--argo-values-reviewed).
	ArgoValuesReviewed string
	// ArgoParallelism and ArgoNamespaceParallelism set the managed Argo
	// controller's workflow limits (controller.parallelism and
	// controller.namespaceParallelism); nil keeps the release's value.
	ArgoParallelism *int

	// argoProfile is the nonroot controller profile the Argo phase rendered
	// (#1517): the built-in config plus the effective controller limits.
	argoProfile               *argoworkflow.NonrootProfile
	ArgoVaultAddress          string
	ArgoVaultCredentialedRole string
	// CredentialedSecretPaths are exact secret paths for the shared
	// credentialed Vault policy, populated from the --credentialed-secret-path
	// flag. When per-repo identities exist (len(PerRepoIdentities) > 0),
	// these are not included in the shared policy, which is stripped to zero
	// stanzas (issue #456); the grants are carried by per-repo policies
	// instead.
	CredentialedSecretPaths []string
	// PerRepoIdentities describes per-repo Vault identities to create.
	// Each entry results in a ServiceAccount (via the chart), a Vault
	// policy, and a Vault role (via ConfigurePerRepoIdentities). The
	// installer populates this from the repo registry + approval table.
	PerRepoIdentities []PerRepoIdentity
	// PerStepIdentities describes per-step Vault identities to create.
	// Each entry results in a ServiceAccount (via the chart), a Vault
	// policy, and a Vault role (via ConfigurePerStepIdentities). The
	// installer populates this from the access list's named-step grants.
	// Issue #623, finding 3.
	PerStepIdentities []PerStepIdentity

	// TLSExtraDNSNames and TLSExtraIPs are addresses the generated server
	// certificate must carry beyond the four in-cluster names. A client
	// reaching the server on any other address gets a hostname mismatch, which
	// is the one TLS failure no trust anchor can repair, so naming the address
	// is a deployment decision rather than something a client works around.
	// Ignored when the chart adopts an existing TLS Secret.
	TLSExtraDNSNames  []string
	TLSExtraIPs       []string
	TLSRemoveDNSNames []string
	TLSRemoveIPs      []string

	// ValuesFiles are helm values files applied to the Oberth release, in the
	// order given, so a deployment can express its own settings at install time
	// rather than reconciling afterwards with a second command. Applied before
	// the installer's own --set flags, which therefore still win: a value this
	// process computed, such as the address of the secret store it just
	// created, must not be replaced by a stale one in a file.
	ValuesFiles []string
	// NetworkPolicy controls pipeline egress NetworkPolicy enforcement:
	// "auto" enables for a new release, including k3s, and preserves a live
	// release's choice. "true" forces on; "false" explicitly disables isolation.
	NetworkPolicy           string
	PushBannerURL           string
	PushBannerURLSet        bool
	AcceptWitnessGenesisSet bool
	existingValuesRead      bool
	existingRelease         bool
	installValuesRequested  bool
	liveInstallValues       map[string]any
	// ServiceType is the Oberth Service type: NodePort, LoadBalancer or
	// ClusterIP. Empty means NodePort on a fresh install and whatever the
	// existing release has on an upgrade.
	ServiceType string
	// pendingCertRotation holds the deferred certificate rotation state.
	// Phase 1 of the identity preflight computes the rotation but does NOT
	// write to OpenBao; phase 2 (commitServerIdentityRotation) writes after
	// the Helm SSA preview confirms no conflicts.
	pendingCertRotation *pendingRotation
	// serverPKI is set by Run for a production secret store: the server then
	// issues its leaf from the installer-managed CA (ensureServerPKI), so the
	// self-signed bundle certificate's preflight checks stand down.
	serverPKI bool
	// rotatedServerCertSHA256 is set once a rotation is committed to OpenBao.
	// It becomes a pod annotation: the server reads its identity only at
	// startup, and a rotation that changes no other value would otherwise
	// leave the pod serving the retired certificate.
	rotatedServerCertSHA256 string

	policyProbePlan bool

	// ChartPath installs the Oberth chart from a local directory or archive
	// instead of the published repository (--chart). This is the loop the
	// rollout depends on: build the server image, install the working tree's
	// chart, push, watch it run, change something, repeat — without a release
	// in between.
	ChartPath string
	// ImageRef overrides the server image the chart deploys (--image), for the
	// same loop. A tag that exists only in the node's image store is the normal
	// case here, so the chart's IfNotPresent pull policy is what makes it work.
	// Without ChartPath it must be the server image the signed release pins.
	ImageRef string
	// Release is where the signed release is read from when ChartPath is
	// empty; the chart is deployed only after it verified.
	Release ReleaseSource
	// verifiedChart and verifiedImage are the private chart file of the
	// verified signed release and the server image its record pins;
	// verifiedVersion is the ChartVersion they were verified for.
	verifiedChart   string
	verifiedImage   string
	verifiedVersion string
	// UtilityImage overrides the chart file-tool image; empty preserves values files and chart defaults.
	UtilityImage  string
	Timeout       time.Duration
	BinaryVersion string

	// ForgeType is the forge platform selected by the TUI wizard (codeberg,
	// github, forgejo, gitlab). Present only when entering onboarding from
	// the wizard; the interactive CLI path prompts instead.
	ForgeType string
	// ForgeOrg is the forge organization (e.g., "acme") from the TUI
	// wizard.
	ForgeOrg string
	// ForgeURL is the upstream URL derived from ForgeType and ForgeOrg
	// (e.g., "github.com/acme"). When set, onboarding uses it
	// instead of prompting.
	ForgeURL string
	// UpstreamName overrides the name derived from the first forge host.
	UpstreamName string
	// UplinkIdentity is the uplink identity (e.g., "admin@workstation") from the
	// TUI wizard. When set alongside SSHPublicKeyPath, onboarding uses it
	// instead of prompting.
	UplinkIdentity string
	// SSHPublicKeyPath is the path to the operator's SSH public key
	// (e.g., "~/.ssh/id_ed25519.pub") from the TUI wizard.
	SSHPublicKeyPath string
	// AcceptWitnessGenesis is the one-shot "<auditID>:<sha256hex>" acknowledgment
	// forwarded to auditAnchor.acceptWitnessGenesis when --install-rekor retrofits
	// an existing deployment that already has audit history.
	AcceptWitnessGenesis string
	// ClientAccess decides the client-access offer in advance (both, cli,
	// mcp, none; --client-access). The setup wizard answers it on its own
	// screen and runs the installer without a terminal, where the offer could
	// otherwise only be skipped. Empty keeps the interactive prompt.
	ClientAccess string
	// ClientHost is the address clients use to reach this server
	// (--client-host): a DNS name or IP. The client files, the SSH config,
	// the push banner and every printed URL name it, and the server
	// certificate always covers it. Empty keeps the default: the Kubernetes
	// API host, or localhost when that is a loopback address (#061).
	ClientHost string
	// ReplaceClientFiles lets an install without a terminal replace client
	// files (~/.config/oberth/{ca.crt,env,mcp.json}) that exist with other
	// content (--replace-client-files). On a terminal the offer is the
	// consent; the setup wizard sets it, because its client-access step is.
	ReplaceClientFiles bool
	// AlsoUpstreams are further organizations onboarding connects with the
	// same upstream key (--also-upstream, host/organization or an ssh:// URL),
	// after the first one (#060).
	AlsoUpstreams []string
	// AssistEnabled turns on agent-assisted continuation (assist.enabled),
	// which is off by default; see docs/assisted-continuation.md.
	AssistEnabled bool
	// ExpectedHostFingerprint pins the first upstream's SSH host key
	// (SHA256:...) so a non-interactive onboarding can trust a forge whose
	// key Oberth does not ship, without a prompt.
	ExpectedHostFingerprint string
	// ClientSSHPort and ClientHTTPSPort are the ports clients reach the
	// server on when they differ from the Service's (a port forward, a
	// container runtime publishing the NodePorts elsewhere). Client files, the
	// push banner and printed commands use them; empty means the Service's.
	ClientSSHPort   string
	ClientHTTPSPort string
	// SecretsFile receives the credentials the install creates once (OpenBao
	// root token and unseal key, the first uplink's bearer token) instead of
	// the output. New credentials go outside any git worktree. An existing
	// production-store file is recovery input; new output uses a new sibling.
	SecretsFile       string
	ResumeSecretsFile string
	ResumeAgeIdentity string
	resumeRootToken   []byte
	secretsFileInput  bool
	// AdminSSHPublicKeyPath explicitly provisions the first admin from a public
	// key generated on the client machine. '-' reads the key from stdin.
	AdminSSHPublicKeyPath string
	AdminCSRPath          string
	AdminBearer           bool
	adminCSR              []byte
	SecretsAgeRecipient   string
	adminSSHPublicKey     []byte
	uplinkOnboarded       bool
	adminToken            string
}

// wantsSecretStore reports whether either secret-store mode was requested.
func (cfg Config) wantsSecretStore() bool {
	return cfg.InstallSecretStore || cfg.InstallSecretStoreDev
}

// promptSecretStoreChoice proposes installing OpenBao when no explicit
// --install-secretstore or --install-secretstore-dev flag was passed. In an
// interactive terminal it offers production setup or cancellation; a fresh
// non-interactive session fails with guidance (credentials printed during production
// setup must never land in CI logs or transcripts unsolicited).
func promptSecretStoreChoice(ctx context.Context, cfg *Config, deps Deps) error {
	if cfg.wantsSecretStore() {
		return nil
	}
	// Read the live release before prompting. A re-run must neither reinstall
	// OpenBao nor silently turn its PKI mode off. Listing failures are errors,
	// not evidence that this is a fresh install.
	if deps.RunHelm != nil {
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		out, err := deps.RunHelm(ctx, helmReleaseListArgs(ns))
		if err != nil {
			return fmt.Errorf("inspect existing secret-store configuration: %w", err)
		}
		var releases []helmRelease
		if err := json.Unmarshal(out, &releases); err != nil {
			return fmt.Errorf("inspect existing release: %w", err)
		}
		for _, release := range releases {
			if release.Name != "oberth" {
				continue
			}
			live, err := readLiveHelmValuesSubset(ctx, deps, ns)
			if err != nil {
				return fmt.Errorf("read existing secret-store configuration: %w", err)
			}
			if strings.TrimSpace(live.Secretstore.Address) == "" {
				return errors.New("existing Oberth release has no secretstore.address; configure its OpenBao connection or pass --install-secretstore")
			}
			cfg.serverPKI = live.TLS.PKI.Enabled
			return nil
		}
	}
	interactive := deps.IsTerminal != nil && deps.IsTerminal() && deps.Input != nil
	if !interactive {
		return errors.New("OpenBao is required for server identities; re-run with --install-secretstore (or --install-secretstore-dev for evaluation)")
	}

	_, _ = fmt.Fprintln(deps.Output, "\nInstall OpenBao for server identities and pipeline secrets?")
	_, _ = fmt.Fprintln(deps.Output, "  [1] Production mode (persistent storage, verified TLS) (Recommended)")
	_, _ = fmt.Fprintln(deps.Output, "  [2] Cancel installation")

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
			return errors.New("installation cancelled; OpenBao is required for server identities")
		default:
			_, _ = fmt.Fprintln(deps.Output, "Please enter 1 or 2.")
			continue
		}
		break
	}
	if !chosen {
		return errors.New("no secret store selected; re-run with --install-secretstore")
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
	// Host installs verify the advertised endpoint after server readiness.
	checkClientReachability bool

	policyProbeCacheDir func() (string, error)
	ProbeNetworkPolicy  func(context.Context, Config, Deps) NetworkPolicyProbeResult
	baoToken            string
	baoTokenCaptured    bool
	// ClientConfigRoot overrides where this machine's CLI/MCP client files
	// live (default ~/.config/oberth); tests point it at their own directory.
	ClientConfigRoot func() (string, error)
	// UserHomeDir isolates host configuration in tests. Nil uses os.UserHomeDir.
	UserHomeDir func() (string, error)
	Output      io.Writer
	Input       io.Reader
	RunHelm     func(ctx context.Context, args []string) ([]byte, error)
	RunCommand  CommandRunner
	// RunCommandStream supplies live progress for the bounded upgrade idle wait.
	RunCommandStream func(context.Context, io.Writer, string, ...string) error
	RunInteractive   InteractiveRunner
	// PodLogs returns one pod's bounded bootstrap output. When nil, the
	// Kubernetes REST client is used directly.
	PodLogs    func(ctx context.Context, namespace, pod string) ([]byte, error)
	IsTerminal func() bool
	// LookPath resolves a host binary, so a step that adapts to what is
	// installed can be tested without depending on the test machine's PATH.
	// Nil selects exec.LookPath.
	LookPath   func(string) (string, error)
	KubeClient kubernetes.Interface
	RestConfig *rest.Config
	// ClientHost is Config.ClientHost, copied by Run so every step that
	// advertises an address (client files, SSH config, next steps) uses it.
	ClientSSHPort     string
	ClientHTTPSPort   string
	ClientServiceType string
	ClientHost        string
	ContextName       string
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
	// CredentialSink, when non-nil, receives every held once-only credential
	// (label, value) instead of the boxed terminal flush — see
	// InstallDeps.CredentialSink. Output-stream text must never be the
	// delivery channel for a secret when this is set.
	CredentialSink func(label, value string)
	// credentialsAtOnce hands each credential to CredentialSink the moment it
	// exists instead of after the install table closes. Set for the secrets
	// file, which is written and synced per credential.
	credentialsAtOnce bool
	// StepProgressSink, when non-nil, receives structured step completion
	// events for TUI progress tracking. Step names are stable identifiers
	// (e.g., "deploy oberth", "rollout ready") matched by the TUI's
	// stepNameToIndex map. Status is "done".
	StepProgressSink func(step, status string)
	UplinkCreated    func(identity string)
	upstreamResult   func(InstallUpstream)
	// ReleaseGetter fetches published release objects for oberth upgrade;
	// nil selects HTTPS (releaseverify.HTTPSGetter).
	ReleaseGetter releaseverify.Getter
	// releaseKey replaces the compiled-in release key in tests.
	releaseKey *ecdsa.PublicKey
}

// ClusterInfo describes the target Kubernetes cluster.
type ClusterInfo struct {
	NodesUnavailable bool
	Context          string
	Server           string
	Engine           string // "k3s", "kind", "k0s", "minikube", "unknown"
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
	// PKIEnabled retains live PKI enablement during a nonmutating Helm preview.
	PKIEnabled bool
	// ServerCACertPEM is the server PKI CA; non-empty enables tls.pki so the
	// server issues its own short-lived leaf. Clients pin this certificate.
	ServerCACertPEM string
}

// SecretStoreResult describes the outcome of secretstore configuration.
type SecretStoreResult struct {
	RekorPublicKey         string
	AuthMountConfigured    bool
	TransitMountEnabled    bool
	TransitKeyCreated      bool
	PolicyWritten          bool
	RoleCreated            bool
	TrustedTransitVerified bool
	// ServerCACertPEM is the installer-managed server CA (production only);
	// non-empty means the server PKI role is provisioned.
	ServerCACertPEM string
	// ServerCAReplaced reports a newly generated CA: pinned clients must
	// refresh their copy.
	ServerCAReplaced bool
	Skipped          bool
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
	if cfg.SecretsAgeRecipient != "" && cfg.SecretsFile == "" {
		return errors.New("--secrets-age-recipient requires --secrets-file")
	}
	if err := validateServerTLSFlags(cfg); err != nil {
		return err
	}
	if err := ValidClientAccess(cfg.ClientAccess); err != nil {
		return fmt.Errorf("--client-access: %w", err)
	}
	if cfg.ClientHost != "" {
		host, err := NormalizeClientHost(cfg.ClientHost)
		if err != nil {
			return fmt.Errorf("--client-host: %w", err)
		}

		cfg.ClientHost = host
	}
	if fp := cfg.ExpectedHostFingerprint; fp != "" {
		if cfg.ForgeURL == "" && cfg.ForgeType == "" {
			return errors.New("--expected-host-fingerprint pins the first upstream's host key; pass it with --upstream-url or --forge")
		}
		if !strings.HasPrefix(fp, "SHA256:") || len(fp) < len("SHA256:")+43 || strings.ContainsAny(fp, " \t\r\n") {
			return fmt.Errorf("--expected-host-fingerprint must be an SSH SHA256 fingerprint (SHA256:<43 characters>, as ssh-keygen -lf prints), got %q", fp)
		}
	}
	for flagName, port := range map[string]string{"--client-ssh-port": cfg.ClientSSHPort, "--client-https-port": cfg.ClientHTTPSPort} {
		if port == "" {
			continue
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return fmt.Errorf("%s must be a port number 1-65535, got %q", flagName, port)
		}
	}
	for i, raw := range cfg.AlsoUpstreams {
		_, baseURL, err := upstreamFromInput(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("--also-upstream: %w", err)
		}
		cfg.AlsoUpstreams[i] = baseURL
	}
	if cfg.ForgeURL == "" && (cfg.ForgeType != "" || cfg.ForgeOrg != "") {
		hosts := map[string]string{"github": "github.com", "codeberg": "codeberg.org", "gitlab": "gitlab.com"}
		host, ok := hosts[cfg.ForgeType]
		if !ok || cfg.ForgeOrg == "" || strings.ContainsAny(cfg.ForgeOrg, "/\\?#@ \t\r\n") || cfg.ForgeOrg == "." || cfg.ForgeOrg == ".." {
			return errors.New("provide --forge github, codeberg, or gitlab with --organization; use --upstream-url for other hosts")
		}
		cfg.ForgeURL = "ssh://git@" + host + "/" + cfg.ForgeOrg
	}

	if cfg.UpstreamName != "" {
		if cfg.ForgeURL == "" {
			return errors.New("--upstream-name requires --upstream-url or --forge with --organization")
		}
		if err := gitcache.ValidateUpstreamName(cfg.UpstreamName); err != nil {
			return fmt.Errorf("--upstream-name: %w", err)
		}
	}

	if cfg.UpstreamPrivateKeyPath != "" {
		if err := CheckUpstreamPrivateKey(cfg.UpstreamPrivateKeyPath); err != nil {
			return fmt.Errorf("--upstream-key-file: %w", err)
		}
	}

	if cfg.InstallRekor {
		if cfg.InstallSecretStoreDev {
			return errors.New("rekor requires persistent production OpenBao; dev storage cannot preserve log identity")
		}
		cfg.InstallSecretStore = true
	}

	if cfg.Dev && cfg.Production {
		return errors.New("--dev and --production are mutually exclusive")
	}
	if cfg.Production {
		return errors.New("production mode is not implemented yet; tracked in BACKLOG.md")
	}
	if cfg.InstallSecretStore && cfg.InstallSecretStoreDev {
		return errors.New("--install-secretstore and --install-secretstore-dev are mutually exclusive")
	}
	switch cfg.ServiceType {
	case "", "NodePort", "LoadBalancer", "ClusterIP":
	default:
		return fmt.Errorf("--service-type must be NodePort, LoadBalancer or ClusterIP, not %q", cfg.ServiceType)
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
	if cfg.InstallRekor {
		if cfg.RekorNamespace == cfg.Namespace || cfg.RekorNamespace == cfg.ArgoNamespace || cfg.RekorNamespace == cfg.OpenBaoNamespace {
			return errors.New("rekor requires its own namespace to isolate database credentials")
		}
		if cfg.RekorChartVersion != DefaultRekorChartVersion {
			return errors.New("bundled Rekor requires the verified chart version shipped with this installer")
		}
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
	if cfg.PushBannerURL != "" {
		parsed, err := url.Parse(cfg.PushBannerURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
			return errors.New("--push-banner-url must be an absolute HTTPS URL without credentials or a fragment")
		}
	}
	if (cfg.AcceptWitnessGenesis != "" || cfg.AcceptWitnessGenesisSet) && !cfg.InstallRekor {
		return errors.New("--accept-witness-genesis requires --install-rekor")
	}
	if cfg.AcceptWitnessGenesis != "" {
		wgPattern := regexp.MustCompile(`^[1-9][0-9]{0,18}:[0-9a-f]{64}$`)
		if !wgPattern.MatchString(cfg.AcceptWitnessGenesis) {
			return errors.New("--accept-witness-genesis must be the exact audit chain head as <auditID>:<sha256hex>")
		}
	}
	if cfg.ChartVersion == "" && cfg.BinaryVersion != "" && cfg.BinaryVersion != "dev" && !strings.HasPrefix(cfg.BinaryVersion, "dev-") {
		cfg.ChartVersion = cfg.BinaryVersion
	}

	return nil
}

// resolveInstallClientHost sets the address clients are told to use: kind
// publishes on 127.0.0.1 only, so a kind install advertises localhost.
func resolveInstallClientHost(cfg *Config, deps *Deps, cluster ClusterInfo) {
	deps.ClientHost = cfg.ClientHost

	if warning := LocalNameWarning(deps.ClientHost); warning != "" {
		_, _ = fmt.Fprintf(deps.Output, "NOTE: %s\n", warning)
	}
}

// Run executes the full install flow.
func Run(ctx context.Context, cfg Config, deps Deps) error {
	cfg.installValuesRequested = explicitInstallValues(cfg)
	if err := captureBaoEnvironment(&deps); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	cluster, err := DetectCluster(ctx, deps)
	if err != nil {
		return fmt.Errorf("detect cluster: %w", err)
	}

	// Any cluster the kubeconfig reaches is a valid target; the install names
	// the one it acts on before anything else.
	_, _ = fmt.Fprintf(deps.Output, "Target: %s (%s)\n", cluster.Context, cluster.Server)
	resolveInstallClientHost(&cfg, &deps, cluster)

	if cfg.InstallSecretStoreDev {
		_, _ = fmt.Fprintln(deps.Output, "EVALUATION MODE — not for production. Dev OpenBao state does not survive pod restart.")
	}

	if err := resolveInstallRerunValues(ctx, &cfg, deps, cluster); err != nil {
		return err
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

	cfg.serverPKI = cfg.serverPKI || cfg.InstallSecretStore
	if err := preflightPodSecurity(ctx, cfg, deps, false); err != nil {
		return err
	}

	// The installer's own access path is the NodePorts (30022/30443). A
	// LoadBalancer Service on k3s makes servicelb take host ports 2222 and
	// 443 on the node, invisibly (#049): default fresh installs to NodePort
	// and keep an existing release's choice unless one is requested.

	if cfg.ServiceType == "LoadBalancer" && cluster.Engine == "k3s" {
		_, _ = fmt.Fprintln(deps.Output, "NOTE: service type LoadBalancer on k3s: servicelb binds the Service ports (2222 and 443 by default) on the node's own address; SSH or HTTPS to those host ports reaches Oberth.")
	}

	// The installer advertises clientReachableHost to the CLI and MCP clients
	// (and prints it as the Ready URL), so the certificate must carry it.
	cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = certificateNamesForClientHost(clientReachableHost(deps), cfg.TLSExtraDNSNames, cfg.TLSExtraIPs)
	if err := preserveServerTLSNames(ctx, &cfg, deps); err != nil {
		return err
	}

	// Identity preflight: verify OpenBao bundles and server certificate SANs.
	// Runs on both the real and dry-run paths. On the real path, a needed
	// certificate rotation is COMPUTED but NOT WRITTEN here; the actual
	// write happens in commitServerIdentityRotation after the Helm SSA
	// preview confirms no field-ownership conflicts (#812 remediation 1).
	if err := preflightServerIdentities(ctx, &cfg, deps, cfg.DryRun); err != nil {
		return err
	}

	if cfg.DryRun {
		return printDryRunPlan(cfg, deps.Output, cluster)
	}
	cleanupRelease, err := preflightInstallRelease(ctx, &cfg, deps)
	if err != nil {
		return err
	}
	defer cleanupRelease()
	// Helm --create-namespace cannot attach PSA labels. Create only missing
	// server/pipeline namespaces ourselves, with the required policy explicit.
	if err := preflightPodSecurity(ctx, cfg, deps, true); err != nil {
		return err
	}

	// Before anything is installed: naming an address the kept TLS identity will
	// not gain is silent otherwise, and surfaces weeks later as a hostname
	// mismatch nobody connects back to this run.
	// A pending rotation is about to add them, and the server PKI issues for
	// the current names at every start, so there is nothing to warn of.
	if cfg.pendingCertRotation == nil && !cfg.serverPKI {
		warnCertificateNamesWillNotTakeEffect(deps.Output, cfg,
			certificateNamesNotYetIssued(ctx, cfg, deps))
	}

	w := deps.Output
	color := isColor(deps)

	// Step progress helper — reports structured step completions to the
	// TUI when a sink is wired, no-op otherwise.
	stepDone := func(step string) {
		if deps.StepProgressSink != nil {
			deps.StepProgressSink(step, "done")
		}
	}

	_, _ = fmt.Fprintf(w, "oberth install %s\n", displayVersion(cfg.BinaryVersion))

	var creds heldCredentials
	if deps.credentialsAtOnce {
		creds.atOnce = deps.CredentialSink
	}

	// Safety net: if any later step errors out or panics, captured
	// credentials are still released. Registered before the secret-store
	// setup because a fresh `operator init` can succeed while a later step
	// still fails — after init the credentials exist nowhere but this
	// process, and losing them locks the store permanently. Emission goes
	// through emitCredentials so a wired CredentialSink (TUI wizard)
	// receives values structurally instead of through the output stream.
	defer emitCredentials(&creds, deps, w, color)

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
		identityRunner := deps.RunCommand
		if deps.KubeClient != nil {
			_, err := deps.KubeClient.AppsV1().Deployments(ns).Get(ctx, "oberth", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				identityRunner = nil
			}
		}
		produced, produceWarnings, produceErr := ProducePerRepoIdentities(ctx, deps.KubeClient, identityRunner, deps.ContextName, ns)
		for _, warn := range produceWarnings {
			_, _ = fmt.Fprintf(deps.Output, "WARNING: %s\n", warn)
		}
		if err := checkProducedIdentities(cfg.liveInstallValues, "perRepoIdentities", len(produced), produceErr); err != nil {
			return err
		}
		if identityRunner == nil || produceErr != nil {
			if err := checkProducedIdentities(cfg.liveInstallValues, "perStepIdentities", 0, produceErr); err != nil {
				return err
			}
		}
		if produceErr != nil {
			_, _ = fmt.Fprintf(deps.Output, "WARNING: could not read per-repo identities: %v\n", produceErr)
		} else if len(produced) > 0 {
			cfg.PerRepoIdentities = produced
		}
		// Per-step identities (issue #623, finding 3). Only produce when the
		// per-repo producer succeeded (prune-safety: an empty per-step list
		// from a producer failure must never reach the chart or the sync).
		if len(cfg.PerStepIdentities) == 0 && identityRunner != nil && produceErr == nil {
			stepIds, stepErr := ProducePerStepIdentities(ctx, identityRunner, deps.ContextName, ns)
			if err := checkProducedIdentities(cfg.liveInstallValues, "perStepIdentities", len(stepIds), stepErr); err != nil {
				return err
			}
			if stepErr != nil {
				_, _ = fmt.Fprintf(deps.Output, "WARNING: could not read per-step identities: %v\n", stepErr)
			} else if len(stepIds) > 0 {
				cfg.PerStepIdentities = stepIds
			}
		}
	}

	stepDone("render chart")

	// --- Helm SSA preview: detect field-ownership conflicts BEFORE any
	// OpenBao write, Argo install, or cert rotation (#812 remediation 1) ---
	// The preview uses the same Helm args the real upgrade will produce,
	// after the identity preflight has computed any pending rotation
	// (either from a pending rotation or from the idempotent pin). If the
	// preview reports SSA conflicts, the installer refuses and prints the
	// exact ownership-transfer commands. Nothing has been mutated at this
	// point.
	if err := previewHelmUpgrade(ctx, cfg, deps, openbao); err != nil {
		return err
	}

	// --- Failed-revision detection (#812 remediation 3) ---
	// A failed latest Helm revision means --reuse-values pulls values from
	// the previous deployed revision, silently dropping any values that were
	// present only in the failed attempt (e.g. a rotated originCACert).
	// Refuse and print guidance instead of compounding the failure.
	{
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		if err := detectFailedHelmRevision(ctx, deps, ns); err != nil {
			return err
		}
	}

	if err := ensurePlatformPriorities(ctx, cfg, deps); err != nil {
		return err
	}

	// --- Commit the deferred cert rotation (#812 remediation 1) ---
	// The Helm SSA preview confirmed no conflicts; it is now safe to write
	// the rotated certificate to OpenBao.
	if err := commitServerIdentityRotation(ctx, &cfg, deps); err != nil {
		return err
	}

	if cfg.wantsSecretStore() {
		quietDeps := deps
		quietDeps.Output = io.Discard
		openbao, err = InstallOpenBao(ctx, cfg, quietDeps)
		if err != nil {
			return fmt.Errorf("install OpenBao: %w", err)
		}
		stepDone("install openbao")

		if cfg.InstallSecretStoreDev {
			if err := SetupDevSecretStore(ctx, cfg, quietDeps, openbao); err != nil {
				return fmt.Errorf("set up dev secret store: %w", err)
			}
			stepDone("secretstore server")
			stepDone("secretstore release") // No release leg in dev mode — mark done so the TUI step tracker resolves.
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
			openbao.ServerCACertPEM = configured.ServerCACertPEM
			if configured.ServerCAReplaced {
				_, _ = fmt.Fprintln(w, "Server CA issued: clients that pinned the previous server certificate or CA must refresh it (oberth install rewrites ~/.config/oberth/ca.crt).")
			}
			cfg.rekorPublicKey = configured.RekorPublicKey
			cfg.rekorBaoAddress = openbao.ServiceAddress
			cfg.rekorBaoCA = openbao.CACertPEM
			secretStoreItems = configured.Items
			stepDone("secretstore server")
			stepDone("secretstore release")
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
		tw.RegisterContent("Network policy", "disabled (configured)", "⚠ warn")
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
	argo, err := installArgoPinningProfile(ctx, &cfg, quietArgoDeps)
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
	stepDone("apply namespace")
	stepDone("deploy oberth")
	stepDone("tls fingerprints")

	// Network policy warning row.
	if cfg.NetworkPolicy == "false" {
		tw.WriteRow("Network policy", "disabled (configured)", "⚠ warn")
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

	nodes, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		info.Engine = "unknown"
		info.NodesUnavailable = true
		return info, nil
	}
	info.Engine = DetectEngine(nodes.Items)
	return info, nil
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

// openBaoServiceAddress is the in-cluster base URL of the installer-managed
// OpenBao: HTTPS in production mode, plain HTTP only in explicit dev mode.
func openBaoServiceAddress(cfg Config) string {
	ns := cfg.OpenBaoNamespace
	if ns == "" {
		ns = DefaultOpenBaoNamespace
	}
	scheme := "http"
	if cfg.InstallSecretStore {
		scheme = "https"
	}
	return fmt.Sprintf("%s://openbao.%s.svc:8200", scheme, ns)
}

// InstallOpenBao installs OpenBao via the helm binary, in dev mode
// (--install-secretstore-dev) or production mode (--install-secretstore).
func InstallOpenBao(ctx context.Context, cfg Config, deps Deps) (OpenBaoResult, error) {
	ns := cfg.OpenBaoNamespace
	if ns == "" {
		ns = DefaultOpenBaoNamespace
	}
	result := OpenBaoResult{
		Namespace:      ns,
		ServiceAddress: openBaoServiceAddress(cfg),
	}

	if err := refreshHelmRepo(ctx, deps, openBaoRepoName, OpenBaoHelmRepoURL); err != nil {
		return result, fmt.Errorf("add OpenBao helm repo: %w", err)
	}

	release, exists, err := findHelmRelease(ctx, deps, "openbao", ns)
	if err != nil {
		return result, err
	}
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

	dependencyFile, dependencyChanged, cleanupDependency, err := dependencySchedulingValues(ctx, deps, "openbao", ns, "server", controlPlanePriority(cfg), exists)
	if err != nil {
		return result, err
	}
	defer cleanupDependency()
	action := planHelmAction(exists, result.InstalledVersion, result.TargetVersion, release.Status, cfg.Upgrade)
	if action == actionSkip && (requiresTLSMigration || dependencyChanged) {
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

	if _, err := deps.RunHelm(ctx, withDependencyValues(OpenBaoHelmArgs(cfg), dependencyFile)); err != nil {
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
// Trillian log server + signer, and the Rekor server — via the embedded
// OpenBao adaptation of sigstore/rekor, pinned to DefaultRekorChartVersion. The
// stack uses durable MySQL for both Trillian and its search index, and
// OpenBao Transit for a persistent, non-exportable signing identity.
func InstallRekorStack(ctx context.Context, cfg Config, deps Deps) (RekorResult, error) {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}
	result := RekorResult{
		Namespace: ns,
		// Chart service name "rekor-server" (release "rekor" + component
		// "server"), service port 80 → container 3000.
		// In-cluster only; integrity verified via signature against pinned keys (auditanchor/rekor.go).
		ServiceAddress: fmt.Sprintf("http://rekor-server.%s.svc:80", ns),
	}

	if cfg.rekorPublicKey == "" || cfg.rekorBaoAddress == "" || cfg.rekorBaoCA == "" {
		return result, errors.New("rekor requires verified production OpenBao provisioning")
	}
	result.LogPublicKeyPEM = cfg.rekorPublicKey

	release, exists, err := findHelmRelease(ctx, deps, "rekor", ns)
	if err != nil {
		return result, err
	}
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

	dir, err := extractRekorChart()
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	connectionPath := filepath.Join(dir, "connection.json")
	if err := os.WriteFile(connectionPath, []byte(rekorConnectionValues(cfg)), 0600); err != nil {
		return result, err
	}
	args := RekorHelmArgs(cfg, valuesFile.Name())
	args[3] = filepath.Join(dir, "rekorchart")
	args = append(args, "-f", connectionPath, "--timeout", cfg.Timeout.String())
	if _, err := deps.RunHelm(ctx, args); err != nil {
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
		"upgrade", "--install", "rekor", "bundled-rekor",
		"-n", ns, "--create-namespace",
		"--version", chartVersion,
		"-f", valuesPath,
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
//   - OpenBao Transit signer: the private key never leaves the store, and
//     Kubernetes authentication is renewed by a pod-local agent.
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
    containerSecurityContext: {runAsUser: 65533, runAsGroup: 65533, runAsNonRoot: true}
    resources:
      requests:
        memory: 128Mi
      limits:
        memory: 256Mi
  createdb:
    containerSecurityContext: {runAsUser: 65533, runAsGroup: 65533, runAsNonRoot: true}
  logSigner:
    containerSecurityContext: {runAsUser: 65533, runAsGroup: 65533, runAsNonRoot: true}
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
  signer: openbao://rekor-%[1]s
  signerFileSecretOptions: null
  gomemlimit: 400MiB
  resources:
    requests:
      memory: 128Mi
    limits:
      memory: 512Mi
  searchIndex:
    storageProvider: mysql
    mysql:
      envCredentials: []
`, ns)

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

	release, exists, err := findHelmRelease(ctx, deps, "oberth", ns)
	if err != nil {
		return result, err
	}
	cfg.existingRelease = exists

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
	if action == actionSkip && (cfg.InstallRekor || cfg.wantsSecretStore() || cfg.installValuesRequested || explicitInstallValues(cfg)) {
		// Adding or hardening an optional service is a values change even when
		// the chart version is already current. Reconcile at the installed
		// version so an older installer cannot downgrade a newer deployment.
		if canonicalChartVersion(result.InstalledVersion) == "" {
			subject := "requested values"
			if cfg.InstallRekor {
				subject = "Rekor values"
			}
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
	if action == actionUpgrade && exists && strings.TrimSpace(cfg.ChartPath) == "" {
		if err := refuseInstallVersionMove(result.InstalledVersion, cfg.ChartVersion); err != nil {
			return result, err
		}
	}
	if action == actionSkip {
		result.AlreadyInstalled = true
		maybeHintNewerOberthChart(ctx, deps, result)
		return result, nil
	}
	cleanupRelease, err := verifyInstallRelease(ctx, &cfg, deps)
	if err != nil {
		return result, err
	}
	defer cleanupRelease()

	// Pin the server image the signed release record names when no --image
	// was given (an explicit one was already checked against the record).
	// --reuse-values carries the previous release's image.ref forward
	// verbatim, so without this pin a chart upgrade deploys the old image
	// under the new chart version (#466).
	if strings.TrimSpace(cfg.ImageRef) == "" && cfg.verifiedImage != "" {
		cfg.ImageRef = cfg.verifiedImage
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
	if err := refreshHelmRepo(ctx, deps, oberthRepoName, OberthHelmRepoURL); err != nil {
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
	args := []string{
		"upgrade", "--install", "oberth", oberthChart(cfg),
		"-n", ns, "--create-namespace",
	}
	// Before every --set below, so the installer's own values take precedence,
	// and in the order given, because helm resolves overlapping files by
	// argument order and reordering them would invert an override.
	for _, file := range cfg.ValuesFiles {
		args = append(args, "-f", file)
	}
	for _, endpoint := range []struct{ key, value string }{
		{"host", cfg.ClientHost}, {"sshPort", cfg.ClientSSHPort}, {"httpsPort", cfg.ClientHTTPSPort},
	} {
		if endpoint.value != "" {
			value, _ := json.Marshal(endpoint.value)
			args = append(args, "--set-json", "clientEndpoint."+endpoint.key+"="+string(value))
		}
	}
	if image := strings.TrimSpace(cfg.ImageRef); image != "" {
		args = append(args, "--set-string", "image.ref="+image)
	}
	if cfg.AssistEnabled {
		args = append(args, "--set", "assist.enabled=true")
	}
	if cfg.UtilityImage != "" {
		args = append(args, "--set-string", "utilityImage="+cfg.UtilityImage)
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
	if len(cfg.TLSRemoveDNSNames)+len(cfg.TLSRemoveIPs) > 0 {
		dns, _ := json.Marshal(append([]string{}, cfg.TLSExtraDNSNames...))
		ips, _ := json.Marshal(append([]string{}, cfg.TLSExtraIPs...))
		args = append(args, "--set-json", "tls.extraDNSNames="+string(dns), "--set-json", "tls.extraIPs="+string(ips))
	}
	// Derive pushBannerURL from the node address already known at TLS SAN
	// time so git-push feedback includes a clickable watch link (UX-21).
	// Prefer IPs (they are direct), fall back to DNS names.
	bannerHost := cfg.ClientHost
	if bannerHost == "" {
		bannerHost = pushBannerHost(cfg.TLSExtraIPs, cfg.TLSExtraDNSNames)
	}
	if cfg.PushBannerURL != "" || cfg.PushBannerURLSet {
		args = append(args, "--set-string", "pushBannerURL="+strings.ReplaceAll(strings.ReplaceAll(cfg.PushBannerURL, `\`, `\\`), ",", `\,`))
	} else if !cfg.existingRelease && bannerHost != "" {
		bannerPort := cfg.ClientHTTPSPort
		if bannerPort == "" {
			bannerPort = defaultClientHTTPSPort(cfg.ServiceType)
		}
		args = append(args, "--set-string", "pushBannerURL=https://"+URLHost(bannerHost)+":"+bannerPort+"/runs")
	}
	args = append(args, argoOberthHelmValues(cfg, openbao)...)
	if openbao.PKIEnabled || openbao.ServerCACertPEM != "" {
		args = append(args,
			"--set", "tls.pki.enabled=true",
			"--set-string", "tls.pki.mount="+defaultServerPKIMount,
			"--set-string", "tls.pki.role="+defaultServerPKIRole,
			"--set-string", "tls.pki.ttl="+defaultServerLeafTTL,
		)
	}
	if cfg.ServiceType != "" {
		args = append(args, "--set", "service.type="+cfg.ServiceType)
	}
	if cfg.rotatedServerCertSHA256 != "" {
		args = append(args, "--set-string", `podAnnotations.oberth\.ci/server-certificate-sha256=`+cfg.rotatedServerCertSHA256)
	}
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
		if cfg.AcceptWitnessGenesis != "" || cfg.AcceptWitnessGenesisSet {
			args = append(args, "--set-string", "auditAnchor.acceptWitnessGenesis="+cfg.AcceptWitnessGenesis)
		}
	}
	// Override the live network policy only for an explicit choice or a fresh install.
	if cfg.NetworkPolicy == "true" || cfg.NetworkPolicy == "false" {
		args = append(args, "--set", "networkPolicy.enabled="+cfg.NetworkPolicy)
	}
	// Idempotent re-runs and upgrades must not reset values a user set on a
	// previous install (OBERTH-RELEASE-024) — in particular an existing
	// secretstore configuration survives a re-run without
	// --install-secretstore. The --set flags above still override reused
	// values. No-op on first install. This intentionally retains installed
	// chart defaults, including compatibility identities. New chart keys must
	// have template fallbacks; test-chart-upgrade.sh and the real Helm install
	// rerun test cover every key added since v0.17.53 (utilityImage).
	args = append(args, "--reuse-values")
	return args
}

// --- Helm helpers ---

type helmRelease struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Revision  json.RawMessage `json:"revision"`
	Status    string          `json:"status"`
	// Chart is helm's "<chart-name>-<chart-version>" field, e.g.
	// "oberth-0.10.58".
	Chart string `json:"chart"`
}

// Helm 4 removed list's -a/--all flag. Explicit status filters work with both
// Helm 3 and Helm 4 and retain failed, pending and historical release states.
func helmReleaseListArgs(namespace string) []string {
	return []string{"list", "-n", namespace, "-o", "json",
		"--deployed", "--failed", "--pending", "--superseded", "--uninstalling", "--uninstalled"}
}

// findHelmRelease distinguishes a missing release from an unreadable release.
// Include pending and failed revisions so their safety checks cannot be skipped.
func findHelmRelease(ctx context.Context, deps Deps, name, namespace string) (helmRelease, bool, error) {
	if deps.RunHelm == nil {
		return helmRelease{}, false, errors.New("helm runner is required to inspect releases")
	}
	out, err := deps.RunHelm(ctx, helmReleaseListArgs(namespace))
	if err != nil {
		return helmRelease{}, false, fmt.Errorf("list Helm releases in %s: %w", namespace, err)
	}
	var releases []helmRelease
	if err := json.Unmarshal(out, &releases); err != nil {
		return helmRelease{}, false, fmt.Errorf("decode Helm release list: %w", err)
	}
	for _, r := range releases {
		if r.Name == name {
			return r, true, nil
		}
	}
	return helmRelease{}, false, nil
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

// helmChartVersion is the exact version Helm is asked for. Published charts
// are versioned without the tag's "v"; asking for "v0.17.26" made Helm fall
// back to the "closest available version", which silently picks a different
// chart when the exact one is missing. Non-semver values pass through.
func helmChartVersion(raw string) string {
	if canonicalChartVersion(raw) == "" {
		return raw
	}
	return strings.TrimPrefix(raw, "v")
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
			if err := ctx.Err(); err != nil {
				return err
			}
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
	endpoint := withClientEndpoint(cfg, WithServiceEndpoint(nil, ns, Deps{ClientHost: cfg.ClientHost, ClientServiceType: cfg.ServiceType}))
	// The commands name the cluster the install named, never whatever the
	// current context is when they are run.
	target := ""
	if cfg.KubeconfigPath != "" {
		target += " --kubeconfig " + shellQuoteWord(cfg.KubeconfigPath)
	}
	if cfg.ContextName != "" {
		target += " --context " + shellQuoteWord(cfg.ContextName)
	}
	installed := "Oberth is installed."
	if cfg.InstallSecretStore {
		installed = "Oberth is installed and connected to OpenBao."
	}
	if cfg.ServiceType == "ClusterIP" {
		installed += "\nClusterIP is internal only. For workstation access, keep a port-forward running:\n  kubectl -n " + ns + " port-forward service/oberth 30022:2222 30443:443"
	}
	_, err := fmt.Fprintf(w, `
%[2]s

Next steps:

  1. Register the upstream (interactive — generates and persists the deploy key):

       kubectl%[5]s exec -it -n %[1]s deploy/oberth -- \
         /usr/local/bin/oberth upstream add <name> ssh://git@<forge>/<org>

  2. Add your first uplink (prints its bearer token once; store it, and pin
     the CA that oberth client pin prints):

       kubectl%[5]s exec -i -n %[1]s deploy/oberth -- \
         /usr/local/bin/oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub

  3. Clone an existing repository through Oberth:

       git clone ssh://git@%[3]s:%[4]s/<upstream>/<org>/<repo>.git

  4. Generate a pipeline and push:

       cd <repo>
       oberth init
       git add .oberth/ && git commit -m "add oberth pipeline"
       git push origin my-branch

  5. Promote green code to a target branch: a promotion needs a single-use
     grant from an admin uplink, which the first uplink is not. Register one
     for a person, with a key no agent holds, and issue the grant with it:

       kubectl%[5]s exec -it -n %[1]s deploy/oberth -- \
         /usr/local/bin/oberth uplink add --admin - admin@host < admin.pub
       oberth mcp call --token-file <admin token file> promote_grant \
         '{"repo":"<upstream>/<org>/<repo>","sha":"<green sha>","branch":"main"}'

`, ns, installed, URLHost(endpoint.ClientHost), endpoint.ClientSSHPort, target)
	return err
}

func printDryRunPlan(cfg Config, w io.Writer, cluster ClusterInfo) error {
	_, _ = fmt.Fprintf(w, "\nDry-run plan (no cluster changes will be made):\n\n")
	step := 1

	releaseVerified := strings.TrimSpace(cfg.ChartPath) != ""
	printReleaseVerification := func() {
		if releaseVerified {
			return
		}
		releaseVerified = true
		_, _ = fmt.Fprintf(w, "     Verify the signed release %s (release.json, chart and their signature bundles, %s) against the compiled-in release key; refuse on any mismatch\n",
			displayVersion(cfg.ChartVersion), releaseSourceText(cfg.Release))
	}

	pipelineNamespace := cfg.ArgoNamespace
	if pipelineNamespace == "" {
		pipelineNamespace = DefaultArgoNamespace
	}
	_, _ = fmt.Fprintf(w, "\n     Establish Helm-owned classes for the Oberth server and helper Pods %s (value %d, PreemptLowerPriority) and %s (value %d, Never).\n\n", controlPlanePriority(cfg), podpriority.ControlPlaneValue, podpriority.PipelineClass(pipelineNamespace), podpriority.PipelineValue)
	openbaoAddr := ""
	var rekor RekorResult
	if cfg.wantsSecretStore() {
		ns := cfg.OpenBaoNamespace
		if ns == "" {
			ns = DefaultOpenBaoNamespace
		}
		if cfg.InstallSecretStore {
			_, _ = fmt.Fprintf(w, "  %d. Stage verified OpenBao TLS before any listener exists\n", step)
			printReleaseVerification()
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
		_, _ = fmt.Fprintf(w, "     helm repo update %s\n", openBaoRepoName)
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
		_, _ = fmt.Fprintf(w, "     Reuse OpenBao Transit signing key rekor-%s and memory-only database credentials\n", rekorNS)
		rekorArgs := RekorHelmArgs(cfg, "<generated-values.yaml>")
		_, _ = fmt.Fprintf(w, "     helm %s\n", strings.Join(rekorArgs, " "))
		_, _ = fmt.Fprintf(w, "     Requires ~1Gi additional RAM (requests; ~2Gi limits) — recommended for 32GB+ machines\n")
		_, _ = fmt.Fprintln(w)
		rekor = RekorResult{
			// In-cluster only; integrity verified via signature against pinned keys (auditanchor/rekor.go).
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
		_, _ = fmt.Fprintf(w, "     helm repo update %s\n", argoRepoName)
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
	printReleaseVerification()
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

// HelmWithKubeArgs wraps a RunHelm function to prepend --kubeconfig and/or
// --kube-context flags to every invocation, ensuring helm targets the same
// cluster the binary's kubeconfig resolution selected. Commands that do not
// interact with the cluster (repo add, show values, search) ignore these
// flags harmlessly. When kubeArgs is empty the original function is returned
// unchanged.
func HelmWithKubeArgs(runHelm func(ctx context.Context, args []string) ([]byte, error), kubeArgs []string) func(ctx context.Context, args []string) ([]byte, error) {
	if len(kubeArgs) == 0 {
		return runHelm
	}
	return func(ctx context.Context, args []string) ([]byte, error) {
		merged := make([]string, 0, len(kubeArgs)+len(args))
		merged = append(merged, kubeArgs...)
		merged = append(merged, args...)
		return runHelm(ctx, merged)
	}
}

// DefaultRunHelm executes the helm binary with the given arguments.
// Stdout and stderr are captured separately: on success only stdout is
// returned (clean data for JSON/YAML callers), on failure both are included
// for diagnostics. This prevents helm stderr warnings (e.g. kubeconfig
// permission notices on macOS) from corrupting parseable output.
func DefaultRunHelm(ctx context.Context, args []string) (result []byte, resultErr error) {

	args, environment, lease, cleanup, prepareErr := kubeconfig.Command("helm", args, installerChildEnvironment())
	if prepareErr != nil {
		return nil, prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	// #nosec G204 -- the command is the literal helm binary; every argument is
	// constructed by this package from validated flags (no shell, no
	// caller-controlled command word).
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = environment
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease}
	}
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
// user configuration is absent, it reports the private copy command for k3s.
func LoadKubeConfigForContext(contextName string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	return loadKubeConfigForContext(contextName, k3sKubeconfigPath, output)
}

// loadKubeConfigForContext is the testable core: the fresh k3s detection path is
// injected so tests can point it at a temp file or an unreadable path, and
// the standard loading rules are injected so tests can simulate an empty
// default search path (clientcmd.RecommendedHomeFile is a package-level
// variable initialized at program start from the original $HOME, which
// t.Setenv cannot override).
func loadKubeConfigForContext(contextName, k3sFallbackPath string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	return loadKubeConfigWithRules(kubeconfig.Rules(""), contextName, k3sFallbackPath, output)
}

func loadKubeConfigWithRules(rules *clientcmd.ClientConfigLoadingRules, contextName, k3sFallbackPath string, output io.Writer) (kubernetes.Interface, *rest.Config, string, error) {
	client, restConfig, selectedContext, err := loadFromRules(rules, contextName)
	if err == nil {
		return client, restConfig, selectedContext, nil
	}
	if fresh := freshK3sHint(rules, k3sFallbackPath); fresh != nil {
		return nil, nil, "", fresh
	}
	return nil, nil, "", err
}

func loadFromRules(rules *clientcmd.ClientConfigLoadingRules, contextName string) (kubernetes.Interface, *rest.Config, string, error) {
	rawConfig, restConfig, err := kubeconfig.Client(rules, contextName)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load kubeconfig: %w", err)
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

// ResolveHelmKubeArgs returns --kubeconfig and/or --kube-context flags that
// make a helm invocation target the same cluster that LoadKubeConfigForContext
// would select, plus a human-readable description of the resolved target.
//
// The selected context is pinned in Helm's arguments. KUBECONFIG keeps its
// full file list; a root-owned k3s file is never selected implicitly.
func ResolveHelmKubeArgs(contextName string) (helmArgs []string, target string, err error) {
	return resolveHelmKubeArgs(kubeconfig.Rules(""), contextName, k3sKubeconfigPath)
}

// resolveHelmKubeArgs is the testable core of ResolveHelmKubeArgs.
func resolveHelmKubeArgs(rules *clientcmd.ClientConfigLoadingRules, contextName, k3sFallbackPath string) ([]string, string, error) {
	_, _, selected, err := loadKubeConfigWithRules(rules, contextName, k3sFallbackPath, io.Discard)
	if err != nil {
		return nil, "", err
	}
	args := []string{"--kube-context", selected}
	if rules.ExplicitPath != "" {
		args = append([]string{"--kubeconfig", rules.ExplicitPath}, args...)
	}
	return args, "context " + selected, nil
}

// pushBannerHost returns the first usable host for the push banner URL.
// Prefers IPs (they reach the node directly), falls back to DNS names.
// Returns "" when neither is available.
func pushBannerHost(ips, dnsNames []string) string {
	for _, ip := range ips {
		if ip != "" {
			return ip
		}
	}
	for _, name := range dnsNames {
		if name != "" {
			return name
		}
	}
	return ""
}
