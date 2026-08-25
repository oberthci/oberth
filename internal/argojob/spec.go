// Package argojob submits and supervises repository-authored Argo Workflows
// as Oberth's second execution engine.
//
// It is the Argo counterpart of internal/job: same lifecycle (build, create,
// wait, cancel, reconcile), same durable name, same result vocabulary, but the
// executed object is a Workflow CRD interpreted by the Argo controller instead
// of a single Kubernetes Job running the repository's own Go pipeline module.
//
// The submission path never contacts an Argo Server. Workflows are created
// through the plain Kubernetes API with the generated Argo clientset, which is
// the pattern upstream's own examples/example-golang/main.go demonstrates.
package argojob

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8scontent "k8s.io/apimachinery/pkg/api/validate/content"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	runIDAnnotation    = "oberth.ci/run-id"
	refAnnotation      = "oberth.ci/ref"
	identityAnnotation = "oberth.ci/spec-identity"
	// #nosec G101 -- an annotation key, not a credential. The annotation's
	// value is a list of OpenBao paths; on this tier no secret value ever
	// passes through the server at all.
	secretPathsAnnotation = "oberth.ci/declared-secret-paths"

	defaultWorkflowTimeout = 12 * time.Hour
	defaultTTLSeconds      = int32(3600)

	// defaultSourceVolumeSize bounds one run's checkout. It is a source tree,
	// not a build output directory: the pipeline's own volumes hold artifacts.
	defaultSourceVolumeSize = "8Gi"

	// defaultSourceSeedImage holds the run's claim open for the single exec
	// that fills it. It needs a shell and tar; busybox is pinned by digest for
	// the same reason every other image here is.
	defaultSourceSeedImage = "busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74"
)

// Config is the administrator-owned half of the Argo engine. Every field that
// decides trust is here, not in the repository document.
type Config struct {
	// Namespace is where every Workflow and its pods are created. It is
	// deliberately separate from the namespace Oberth itself runs in: the
	// Vault Kubernetes-auth roles bind (namespace, ServiceAccount) pairs, and
	// a pipeline namespace that also contains the server would let a
	// pipeline-tier identity sit next to server-tier ones.
	Namespace string

	// PipelineServiceAccount runs pipelines (branch, promotion, release) whose
	// templates declare no secrets. It should hold no Kubernetes permissions and
	// no Vault role. Its Pods run with automountServiceAccountToken false.
	PipelineServiceAccount string

	// CredentialedServiceAccount runs RELEASE templates that have approved
	// secrets in the secret_access table. It carries a projected token so the
	// credential chain can authenticate to OpenBao. The bound Vault role's
	// policy is derived from the approval table and is the only identity whose
	// policy may carry system-namespace (release) grants.
	CredentialedServiceAccount string

	// CISecretsServiceAccount runs CI-trigger templates that declare approved
	// upstream-scoped secret paths. It is deliberately a separate identity
	// from CredentialedServiceAccount: OpenBao's Kubernetes auth validates
	// (namespace, ServiceAccount) pairs, so binding branch-tier pods to a
	// different ServiceAccount is what makes the release-tier role — and every
	// release grant attached to it — unreachable from a branch push at the
	// Vault layer, independent of the admission gate. Its role's policy covers
	// the upstream subtree only and never receives approval-table grants.
	CISecretsServiceAccount string

	// ExecutorServiceAccount is what Argo's own init and wait containers run
	// as. It holds only the workflowtaskresults permissions the executor needs
	// and no Vault role, so the executor's unavoidable Kubernetes access never
	// becomes the pipeline's.
	//
	// Argo requires it whenever a step container's token is withheld, and it
	// mounts this identity's token into the executor container only. That is
	// what lets a branch-tier step container run with no token at all.
	ExecutorServiceAccount string

	// RunnerImagePrefixes is the same administrator allowlist the Go path
	// applies to a declared RunnerImage.
	RunnerImagePrefixes []string

	// VaultAddress and VaultCredentialedRole are injected into credentialed
	// containers as VAULT_ADDR and OBERTH_VAULT_ROLE so a repository's
	// envconsul invocation never hardcodes either. Both are server-owned: a
	// document naming a different role still cannot use it, because the Vault
	// role is bound to the ServiceAccount Oberth forced.
	VaultAddress          string
	VaultCredentialedRole string

	// VaultCISecretsRole is the OpenBao Kubernetes-auth role CI-trigger
	// credentialed pipelines log in with, bound to exactly (Namespace,
	// CISecretsServiceAccount) with the upstream-only policy. Empty fails
	// closed: a CI pipeline that declares secret paths is refused at
	// admission rather than submitted under the release-tier role.
	VaultCISecretsRole string

	// VaultCACertPEM is the trust anchor a release-tier container verifies the
	// Vault address against. It is delivered as a file into the run's own
	// source claim (see sourcevolume.go) and advertised as VAULT_CACERT, so a
	// repository never states which certificate authority it trusts -- the
	// same reason the address and the role are server-owned.
	//
	// It is public material: a certificate, not a key. Validate refuses a PEM
	// carrying a private key precisely because the flag takes a path, and a
	// combined key-and-certificate file is an easy thing to point it at.
	//
	// Empty means the store presents a publicly trusted certificate and the
	// container's own root store suffices.
	VaultCACertPEM string

	// CICacheRoot and ReleaseCacheRoot are the node-local directories that hold
	// the persistent Go module and build caches pipeline steps reuse between
	// runs. They are the administrator's, exactly like every other field here:
	// Admit refuses a repository-declared hostPath outright, so the only way a
	// pipeline ever reaches node storage is because the server put it there.
	//
	// The two roots are separate because a cache is writable state that outlives
	// the run that wrote it, which makes it the one place a branch build could
	// otherwise hand bytes to a signed release. Keeping the tiers on different
	// directories means that is not a policy enforced at read time but a path
	// the other tier is never told about.
	//
	// Empty disables the cache for that tier: the run still succeeds, it just
	// starts cold. Nothing downstream is conditional on a cache existing.
	CICacheRoot      string
	ReleaseCacheRoot string

	// SourceVolumeSize, SourceStorageClass, and SourceSeedImage describe the
	// per-run claim the server creates in the pipeline namespace and fills
	// with this revision's checkout.
	//
	// That claim is the one PersistentVolumeClaim an Argo-authored pipeline
	// ever mounts, and the repository cannot ask for it: Admit refuses every
	// claimName reference precisely so the server owns this decision. The
	// mount is read-only and the claim belongs to one run, so one run cannot
	// read or corrupt another's workspace.
	//
	// The claim is ReadWriteOnce, which is correct on a single-node cluster
	// (the deployment target) and would need ReadWriteMany to spread a
	// pipeline's steps across nodes.
	SourceVolumeSize   string
	SourceStorageClass string
	// SourceSeedImage holds the claim open for the one exec that fills it. It
	// needs a shell and tar and nothing else.
	SourceSeedImage string
	// SeedPollInterval overrides the wait cadence for the seeding Pod in tests.
	SeedPollInterval time.Duration
	// OrphanClaimGrace overrides how long an unowned source claim survives
	// before the sweep collects it. Zero selects one hour.
	OrphanClaimGrace time.Duration

	// WorkflowTimeout caps the document's own spec.activeDeadlineSeconds.
	WorkflowTimeout time.Duration

	// TTLSeconds is how long a finished Workflow object is retained.
	TTLSeconds int32

	// MaxRunLogBytes bounds the aggregate bytes written across all steps of
	// one run log. Without this ceiling, a pipeline with 512 admitted steps
	// could write up to 512 * 32 MiB = 16 GiB to the shared server PVC.
	// Default: 64 MiB.
	MaxRunLogBytes int64
}

func (config *Config) applyDefaults() {
	if config.WorkflowTimeout <= 0 {
		config.WorkflowTimeout = defaultWorkflowTimeout
	}
	if config.TTLSeconds <= 0 {
		config.TTLSeconds = defaultTTLSeconds
	}
	if strings.TrimSpace(config.SourceVolumeSize) == "" {
		config.SourceVolumeSize = defaultSourceVolumeSize
	}
	if strings.TrimSpace(config.SourceSeedImage) == "" {
		config.SourceSeedImage = defaultSourceSeedImage
	}
	if config.MaxRunLogBytes <= 0 {
		config.MaxRunLogBytes = 64 << 20 // 64 MiB, matching the Go engine's default
	}
}

// Validate rejects a configuration that could not produce a safe submission.
func (config Config) Validate() error {
	var problems []error
	if messages := k8svalidation.IsDNS1123Label(config.Namespace); len(messages) != 0 {
		problems = append(problems, fmt.Errorf("argojob: namespace %q is invalid: %s",
			config.Namespace, strings.Join(messages, ", ")))
	}
	accounts := map[string]string{
		"pipeline ServiceAccount":     config.PipelineServiceAccount,
		"credentialed ServiceAccount": config.CredentialedServiceAccount,
		"ci-secrets ServiceAccount":   config.CISecretsServiceAccount,
		"executor ServiceAccount":     config.ExecutorServiceAccount,
	}
	for name, account := range accounts {
		if messages := k8svalidation.IsDNS1123Subdomain(account); len(messages) != 0 {
			problems = append(problems, fmt.Errorf("argojob: %s %q is invalid: %s",
				name, account, strings.Join(messages, ", ")))
		}
	}
	// Any shared identity erases a boundary: pipeline==credentialed defeats
	// the secret access gate, ci-secrets==credentialed collapses the branch
	// and release trust tiers back into one Vault-visible identity, and
	// executor==any pipeline identity hands step containers the Kubernetes
	// access the executor needs for bookkeeping.
	distinct := map[string]struct{}{}
	for _, account := range accounts {
		if account == "" {
			continue
		}
		if _, duplicate := distinct[account]; duplicate {
			problems = append(problems, fmt.Errorf(
				"argojob: the pipeline, credentialed, ci-secrets, and executor ServiceAccounts must all differ; %q is used more than once", account))
		}
		distinct[account] = struct{}{}
	}
	if err := periapsis.ValidateRunnerImagePrefixes(config.RunnerImagePrefixes); err != nil {
		problems = append(problems, fmt.Errorf("argojob: %w", err))
	}
	if config.VaultAddress != "" && !strings.HasPrefix(config.VaultAddress, "https://") {
		// The pod authenticates to Vault with its ServiceAccount token and
		// receives secret material back. Plain HTTP would put both on the wire.
		problems = append(problems, fmt.Errorf("argojob: Vault address %q must be https://", config.VaultAddress))
	}
	if err := validateVaultCACertPEM(config.VaultCACertPEM); err != nil {
		problems = append(problems, err)
	}
	if strings.TrimSpace(config.VaultCACertPEM) != "" && config.VaultAddress == "" {
		problems = append(problems, errors.New(
			"argojob: a Vault CA certificate without a Vault address configures a trust anchor for a store no pipeline is told to reach"))
	}
	// VaultAddress and VaultCredentialedRole must come as a pair: one without
	// the other configures a credentialed pipeline that cannot complete a login.
	hasAddr := strings.TrimSpace(config.VaultAddress) != ""
	hasRole := strings.TrimSpace(config.VaultCredentialedRole) != ""
	if hasAddr != hasRole {
		problems = append(problems, fmt.Errorf(
			"argojob: Vault address and credentialed role must both be set or both be empty; got address=%q, role=%q",
			config.VaultAddress, config.VaultCredentialedRole))
	}
	// The CI-secrets role without an address is the same misconfiguration in
	// the other direction. The address WITHOUT a CI role is legal — it means
	// this deployment admits no CI-trigger credentialed pipelines, and Build
	// fails such a submission closed at admission with the flag to set.
	if strings.TrimSpace(config.VaultCISecretsRole) != "" && !hasAddr {
		problems = append(problems, fmt.Errorf(
			"argojob: a CI-secrets Vault role (%q) without a Vault address configures a login no pipeline is told how to reach",
			config.VaultCISecretsRole))
	}
	problems = append(problems, validateCacheRoots(config.CICacheRoot, config.ReleaseCacheRoot)...)
	return errors.Join(problems...)
}

// validateCacheRoots refuses a cache configuration that would erase the tier
// boundary or point a hostPath somewhere unintended.
//
// The chart already enforces these rules in validations.yaml, but the flags
// exist independently of the chart and a hostPath is the one field here that
// names a directory on the node. Checking it in Go as well means a hand-run
// server cannot be configured into the shape the chart refuses to render.
//
// Nesting is rejected rather than normalised: one root inside the other is a
// configuration that looks like two tiers and behaves like one, which is the
// exact failure the split exists to prevent.
func validateCacheRoots(ciRoot, releaseRoot string) []error {
	var problems []error
	roots := map[string]string{
		"--ci-cache-root":      strings.TrimSpace(ciRoot),
		"--release-cache-root": strings.TrimSpace(releaseRoot),
	}
	for _, name := range []string{"--ci-cache-root", "--release-cache-root"} {
		root := roots[name]
		if root == "" {
			continue
		}
		if !strings.HasPrefix(root, "/") || path.Clean(root) != root {
			problems = append(problems, fmt.Errorf(
				"argojob: %s %q must be a clean absolute path", name, root))
		}
	}
	ci, release := roots["--ci-cache-root"], roots["--release-cache-root"]
	if ci == "" || release == "" {
		return problems
	}
	if ci == release ||
		strings.HasPrefix(ci+"/", release+"/") ||
		strings.HasPrefix(release+"/", ci+"/") {
		problems = append(problems, fmt.Errorf(
			"argojob: the CI cache root %q and the release cache root %q must be distinct and non-nested; "+
				"a shared cache would let a branch build persist bytes a release later reuses", ci, release))
	}
	return problems
}

// validateVaultCACertPEM fails a trust anchor that could not verify anything,
// or that carries more than an anchor.
//
// The private-key check is not theoretical: the flag behind this field names a
// file, and the conventional way to hold a serving identity is one PEM file
// with the key and the certificate in it. Streaming that into every release
// Pod would hand pipelines the store's own identity, and nothing downstream
// would notice, because a bundle with a key in it verifies exactly as well.
func validateVaultCACertPEM(pemText string) error {
	if strings.TrimSpace(pemText) == "" {
		return nil
	}
	var certificates int
	for rest := []byte(pemText); len(rest) != 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE":
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return fmt.Errorf("argojob: Vault CA certificate is not a parseable certificate: %w", err)
			}
			certificates++
		case strings.Contains(block.Type, "PRIVATE KEY"):
			return errors.New("argojob: the Vault CA bundle contains a private key; it is delivered to every release Pod and must hold certificates only")
		}
	}
	if certificates == 0 {
		return errors.New("argojob: the Vault CA bundle contains no PEM certificate")
	}
	return nil
}

// internalHostSuffixes are DNS suffixes a publicly trusted certificate
// authority is not permitted to issue for. The CA/Browser Forum baseline
// requirements forbid issuance for internal server names, so a name ending in
// one of these cannot be served by a certificate a stock root store verifies,
// however the endpoint behind it is configured.
var internalHostSuffixes = []string{".svc", ".cluster.local", ".local", ".internal", ".localdomain"}

// vaultAddressRequiresPinnedAnchor reports whether the configured Vault address
// names an endpoint whose serving certificate no public CA may issue, so a
// release-tier container's stock root store can never verify it.
//
// An empty VaultCACertPEM is documented to mean "the store presents a publicly
// trusted certificate and the container's own root store suffices". For a
// cluster-internal Service name that reading is not merely optimistic, it is
// impossible: the only certificate such an endpoint can present is one from a
// private CA, and without the anchor every credentialed release step fails its
// login with "x509: certificate signed by unknown authority" -- after the whole
// build has already run. This is what lets that be said at admission instead.
func vaultAddressRequiresPinnedAnchor(address string) bool {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		// Every literal address, not only the reserved ranges. A store reached
		// by IP is an in-cluster arrangement, and on the rare deployment where
		// that address really does hold a publicly trusted certificate the
		// remedy is to pin that public CA -- which verifies exactly as well and
		// weakens nothing. Guessing the other way costs a release.
		return true
	}
	if !strings.Contains(host, ".") {
		// A single-label name resolves only through a search domain, which is
		// to say only inside the cluster.
		return true
	}
	for _, suffix := range internalHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Request is one run's submission intent.
type Request struct {
	RunID string
	// Name is the durable, deterministic object name the scheduler already
	// persisted for this run. Both engines use it, so recovery can find the
	// object without knowing which engine created it.
	Name        string
	Repo        string
	UpstreamOrg string
	Ref         string
	SHA         string
	Trigger     periapsis.Trigger
	// Source is the exact bytes of the repository's pipeline document for
	// this trigger, read from the immutable run workspace.
	Source []byte
	// SourceDir is the server-side checkout of this exact revision, on the
	// server's own volume. It is the bytes the seeder copies.
	SourceDir string
	// SourceVolume is the per-run claim, in the pipeline namespace, that the
	// server seeded with this revision's checkout. A Pod can only mount claims
	// in its own namespace, so the server's own claim is not reachable from
	// here; see sourcevolume.go for why the copy exists at all.
	SourceVolume SourceVolume

	// ApprovedSecrets is the set of secret paths this repository has active
	// approval-table grants for. Build checks each declared path against
	// this set. The map key is the full declared path (e.g.
	// "oberth/upstream/skipops/oberth/test-secret"). Pre-loaded by the
	// caller via store.ActiveSecretGrants before calling Build.
	//
	// Must be non-nil. An empty map means no grants; a nil map is a
	// programming error (the caller forgot to load the approval table).
	ApprovedSecrets map[string]bool
}

// Build turns a repository document into the exact Workflow object Oberth will
// submit: decoded, admitted, identity-forced, and stamped with the server's own
// metadata and bounds.
//
// It needs no cluster, exactly like internal/job.Build, so the whole admission
// chain is testable from a directory. Its one filesystem dependency is
// deliberate: envconsul admission reads the repository's own credential
// configuration out of the immutable run workspace named by
// request.SourceDir, because those files — not the document — decide what a
// legacy-chain step fetches.
func Build(config Config, request Request) (*wfv1.Workflow, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := request.validate(); err != nil {
		return nil, err
	}
	workflow, err := argoworkflow.Decode(request.Source)
	if err != nil {
		return nil, err
	}
	declaredPaths, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		return nil, err
	}
	if request.ApprovedSecrets == nil {
		return nil, errors.New("argojob: ApprovedSecrets must be non-nil; the caller must load the approval table before calling Build")
	}
	if err := authorizeWithApprovalTable(declaredPaths, request); err != nil {
		return nil, err
	}

	// A credentialed pipeline is one that declares secret-store paths it will
	// read in-Pod. The trigger type is not a factor: a release with no
	// declared paths runs as a plain pipeline identity (no token, no vault
	// wiring), just like a CI pipeline without paths. This is the single
	// signal that selects the ServiceAccount, the projected token, the vault
	// environment, and the trust anchor mount -- all keyed off one boolean.
	credentialed := len(declaredPaths) > 0

	// Refuse a credentialed run the deployment cannot possibly credential, at
	// admission, rather than letting the whole build run and die on the first
	// step that needs the store. The check is on the configured anchor and not
	// on this run's seeding, so it does not depend on the order Create does
	// its work in: an empty VaultCACertPEM means no run will ever carry one.
	//
	// A CI pipeline without secret-store paths receives no token and contacts
	// no store, so an address it cannot verify costs it nothing.
	if credentialed &&
		strings.TrimSpace(config.VaultCACertPEM) == "" &&
		vaultAddressRequiresPinnedAnchor(config.VaultAddress) {
		return nil, fmt.Errorf("argojob: refusing to submit a %s run for %q: the Vault address %q is a "+
			"cluster-internal endpoint no publicly trusted CA may issue for, and no trust anchor is configured, "+
			"so every credentialed step would fail its login with \"x509: certificate signed by unknown authority\"; "+
			"set argo.vault.caCert (--argo-vault-ca-cert) to the PEM of the CA that signed that endpoint",
			request.Trigger, request.Repo, config.VaultAddress)
	}

	identity, err := config.identityFor(request.Trigger, len(declaredPaths) > 0)
	if err != nil {
		return nil, err
	}
	// Prepare admits and binds in one call so the order cannot be got wrong.
	if err := argoworkflow.Prepare(
		workflow,
		argoworkflow.Policy{RunnerImagePrefixes: config.RunnerImagePrefixes},
		identity,
	); err != nil {
		return nil, err
	}
	// Reject duplicate normalized (burn, step) keys before the Workflow is
	// created. The planner's PlannedSteps performs the same check for `oberth
	// validate`, but the scheduler's recordStepPlan deliberately swallows plan
	// errors ("observability record, never a gate"), so without this call a
	// colliding pipeline would still submit and execute — and
	// deduplicateRetries would silently drop one branch's results. Reusing
	// PlannedSteps ensures the two layers share the same normalization.
	if _, err := argoworkflow.PlannedSteps(workflow); err != nil {
		return nil, err
	}
	// A credentialed run without Vault coordinates would proceed through the
	// entire build and die at the first envconsul login — a worse signal than
	// failing at admission. Require both non-empty before any build work. The
	// role is the TRIGGER'S OWN tier's role: a deployment that has never
	// configured the CI-secrets role fails a CI credentialed submission here,
	// closed, instead of submitting it under the release-tier role.
	if credentialed {
		if strings.TrimSpace(config.VaultAddress) == "" {
			return nil, errors.New("argojob: credentialed pipeline requires a Vault address (argo.vault.address / --argo-vault-address)")
		}
		if config.vaultRoleFor(request.Trigger) == "" {
			switch request.Trigger {
			case periapsis.TriggerCI:
				return nil, fmt.Errorf("argojob: refusing the CI run for %q: it declares secret-store paths but no "+
					"CI-secrets Vault role is configured (argo.ciSecrets.vaultRole / --argo-vault-ci-secrets-role); "+
					"a CI pipeline never runs under the release-tier credentialed role", request.Repo)
			default:
				return nil, errors.New("argojob: credentialed pipeline requires a Vault credentialed role (argo.vault.credentialedRole / --argo-vault-credentialed-role)")
			}
		}
	}
	// Admission gate: for every template that uses `oberth secretstore exec`,
	// parse its --path arguments and verify each is in the declared paths set.
	// A wrapper invocation in a workflow with no declared paths is refused: it
	// would run as the pipeline SA with no token and fail at the vault login.
	if err := admitSecretstoreExecPaths(workflow, declaredPaths); err != nil {
		return nil, err
	}
	// Admission gate for the legacy envconsul chain: every secret path its
	// repository-authored configuration would fetch — `secret {}` stanzas in
	// each -config file, -secret command-line flags — must appear in the same
	// declared annotation, read from the immutable run workspace this
	// submission executes. Without this, the annotation gates only the
	// declaration while the fetch obeys the files (issue #200, second
	// manifestation).
	if err := admitEnvconsulSecretPaths(workflow, declaredPaths, request.SourceDir); err != nil {
		return nil, err
	}

	scopeSynchronization(workflow, request)
	applyServerMetadata(workflow, config, request, declaredPaths)
	applyServerBounds(workflow, config)
	applyServerSecurity(workflow)
	injectServerVolumes(workflow, config, request, credentialed)
	injectWorkspaceEnvironment(workflow)
	injectRunEnvironment(workflow, config, request, credentialed)

	digest, err := specIdentity(workflow)
	if err != nil {
		return nil, err
	}
	workflow.Annotations[identityAnnotation] = digest
	return workflow, nil
}

// authorizeWithApprovalTable checks each declared secret path against the
// pre-loaded approval table, preserving upstream namespace scoping and the
// CI system-path prohibition as defense in depth.
//
// The approval table is keyed by (repo, step, secret). Until per-template
// annotations land, the caller passes step="*" grants flattened into a
// path set, so this function only checks path membership. Upstream scoping
// (the declaring repo may only reach its own org/repo namespace) is enforced
// independently of the approval table.
func authorizeWithApprovalTable(paths []string, request Request) error {
	var problems []error
	for _, declared := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(declared)
		if err != nil {
			problems = append(problems, fmt.Errorf("argojob: %w", err))
			continue
		}
		if upstreamScoped {
			if err := scoped.Authorize(request.UpstreamOrg, request.Repo); err != nil {
				problems = append(problems, fmt.Errorf("argojob: %w", err))
				continue
			}
		}
		// CI pipelines may not declare system-namespace paths regardless of
		// what the approval table says. The identity switch already binds CI
		// runs to the ci-secrets ServiceAccount, whose Vault policy covers
		// the upstream subtree only, so admitting the declaration would let
		// the run proceed and fail at Vault read time — a worse signal — and
		// the admission record would misstate what the run could reach.
		if !upstreamScoped && request.Trigger == periapsis.TriggerCI {
			problems = append(problems, fmt.Errorf(
				"argojob: secret store path %q is a system-namespace path; "+
					"CI pipelines may only declare upstream-scoped paths (%s<org>/<repo>/<secret>)",
				declared, periapsis.UpstreamSecretPathPrefix))
			continue
		}
		if !request.ApprovedSecrets[declared] {
			problems = append(problems, fmt.Errorf(
				"argojob: unapproved secret path %q for repo %q; "+
					"approve with: oberth access allow %s '*' %s",
				declared, request.Repo, request.Repo, declared))
		}
	}
	return errors.Join(problems...)
}

// admitSecretstoreExecPaths walks every template in the workflow and, for any
// that invoke `oberth secretstore exec`, checks that every --path argument
// appears in the workflow's declared secret-paths annotation. This makes
// per-step secret intent visible and checkable at admission rather than
// deferring to a vault policy error at runtime.
//
// A workflow with no declared paths that contains an `oberth secretstore exec`
// invocation is refused: the template would run as the pipeline SA with no
// token and fail at vault login, which is a worse signal than a clear
// admission error.
func admitSecretstoreExecPaths(workflow *wfv1.Workflow, declaredPaths []string) error {
	declared := make(map[string]struct{}, len(declaredPaths))
	for _, p := range declaredPaths {
		declared[p] = struct{}{}
	}
	var problems []error
	// Inline templates are visited too: the credential mount injection reaches
	// them, so the admission that governs the same invocation must as well.
	walkTemplates(workflow, func(template *wfv1.Template) {
		if !templateUsesOberthSecretstore(template) {
			return
		}
		execPaths := extractExecPaths(template)
		if len(execPaths) == 0 {
			// A materialize invocation does not declare --path flags; it reads
			// from environment variables set by envconsul. No path check needed.
			return
		}
		if len(declaredPaths) == 0 {
			problems = append(problems, fmt.Errorf(
				"argojob: template %q uses oberth secretstore exec with --path flags "+
					"but the workflow declares no %s annotation",
				template.Name, argoworkflow.SecretPathsAnnotation))
			return
		}
		for _, execPath := range execPaths {
			if _, ok := declared[execPath]; !ok {
				problems = append(problems, fmt.Errorf(
					"argojob: template %q declares --path %q which is not in the "+
						"workflow's %s annotation",
					template.Name, execPath, argoworkflow.SecretPathsAnnotation))
			}
		}
	})
	return errors.Join(problems...)
}

// identityFor selects the ServiceAccount from the trigger AND whether the
// pipeline declares approved secrets. Pipelines without secret paths get the
// pipeline SA (no token) on every trigger. Pipelines with secret paths split
// by trust tier: the release trigger gets the credentialed SA, whose Vault
// role's policy carries the approval-table grants (release credentials); the
// CI trigger gets the ci-secrets SA, whose Vault role's policy covers the
// upstream subtree only and never receives grants.
//
// The split is the Vault-layer half of the trust-tier separation. Admission
// already refuses a CI document that DECLARES a system-namespace path, but a
// pod's projected token can attempt any read its bound role's policy allows,
// declared or not — repository-authored code runs in that pod. Binding CI
// pods to a ServiceAccount the release role does not accept is what makes
// release credentials unreachable from a branch push even then (issue #200).
func (config Config) identityFor(trigger periapsis.Trigger, hasSecretPaths bool) (argoworkflow.Identity, error) {
	if !trigger.Valid() {
		return argoworkflow.Identity{}, fmt.Errorf("argojob: unknown trigger %q", trigger)
	}
	if !hasSecretPaths {
		return argoworkflow.Identity{
			Namespace:                    config.Namespace,
			ServiceAccountName:           config.PipelineServiceAccount,
			ExecutorServiceAccountName:   config.ExecutorServiceAccount,
			AutomountServiceAccountToken: false,
		}, nil
	}
	switch trigger {
	case periapsis.TriggerRelease:
		return argoworkflow.Identity{
			Namespace:                    config.Namespace,
			ServiceAccountName:           config.CredentialedServiceAccount,
			ExecutorServiceAccountName:   config.ExecutorServiceAccount,
			AutomountServiceAccountToken: true,
		}, nil
	case periapsis.TriggerCI:
		// Config.Validate requires the account, so this is a defensive
		// backstop for direct construction: absent configuration must fail
		// the submission, never fall through to the release-tier identity.
		if strings.TrimSpace(config.CISecretsServiceAccount) == "" {
			return argoworkflow.Identity{}, errors.New(
				"argojob: no CI-secrets ServiceAccount is configured (--argo-ci-secrets-serviceaccount); " +
					"refusing to run a CI pipeline with declared secret paths under the release-tier identity")
		}
		return argoworkflow.Identity{
			Namespace:                    config.Namespace,
			ServiceAccountName:           config.CISecretsServiceAccount,
			ExecutorServiceAccountName:   config.ExecutorServiceAccount,
			AutomountServiceAccountToken: true,
		}, nil
	default:
		return argoworkflow.Identity{}, fmt.Errorf("argojob: no credentialed identity exists for trigger %q", trigger)
	}
}

// vaultRoleFor selects the OpenBao Kubernetes-auth role a credentialed run's
// containers are told to log in with, matching the ServiceAccount identityFor
// selected for the same trigger. It is the same shape as cacheRootFor,
// deliberately: one answer to "what tier is this run".
//
// Naming a role is advisory — the pod could name any role — so this is not
// the boundary. The boundary is that each role's bound_service_account_names
// accepts only its own tier's ServiceAccount, which identityFor already
// forced. An unknown trigger returns no role, which fails closed at the
// credentialed-coordinates check in Build.
func (config Config) vaultRoleFor(trigger periapsis.Trigger) string {
	switch trigger {
	case periapsis.TriggerCI:
		return strings.TrimSpace(config.VaultCISecretsRole)
	case periapsis.TriggerRelease:
		return strings.TrimSpace(config.VaultCredentialedRole)
	default:
		return ""
	}
}

func applyServerMetadata(workflow *wfv1.Workflow, config Config, request Request, declaredPaths []string) {
	workflow.APIVersion = argoworkflow.APIVersion
	workflow.Kind = argoworkflow.Kind
	workflow.Name = request.Name
	workflow.GenerateName = ""
	workflow.Namespace = config.Namespace
	if workflow.Labels == nil {
		workflow.Labels = map[string]string{}
	}
	workflow.Labels["app.kubernetes.io/name"] = "oberth-run"
	workflow.Labels["oberth.ci/repo"] = labelValue(request.Repo)
	workflow.Labels["oberth.ci/run"] = labelValue(request.RunID)
	workflow.Labels["oberth.ci/sha"] = labelValue(request.SHA)
	workflow.Labels["oberth.ci/trigger"] = string(request.Trigger)
	workflow.Labels["oberth.ci/engine"] = "argo"
	if workflow.Annotations == nil {
		workflow.Annotations = map[string]string{}
	}
	workflow.Annotations[runIDAnnotation] = request.RunID
	workflow.Annotations[refAnnotation] = request.Ref
	// Pin how the controller names step Pods, so the server can compute the
	// Pod backing each node and stream its log. Left unpinned, the name would
	// follow the controller's own POD_NAMES environment.
	workflow.Annotations[PodNameVersionAnnotation] = podNameVersionV2
	if len(declaredPaths) != 0 {
		// Paths only, never values: this is the auditable statement of intent
		// that the run's audit action records at submission time.
		workflow.Annotations[secretPathsAnnotation] = strings.Join(declaredPaths, ",")
	}
	if workflow.Spec.PodMetadata == nil {
		workflow.Spec.PodMetadata = &wfv1.Metadata{}
	}
	if workflow.Spec.PodMetadata.Labels == nil {
		workflow.Spec.PodMetadata.Labels = map[string]string{}
	}
	workflow.Spec.PodMetadata.Labels["oberth.ci/run"] = labelValue(request.RunID)
	workflow.Spec.PodMetadata.Labels["oberth.ci/trigger"] = string(request.Trigger)
}

// scopeSynchronization rewrites every mutex name to include the trigger and
// repository, so a CI pipeline cannot squat a release-tier lock and two
// different repositories cannot interfere with each other. The rewrite is
// transparent to the repository author: they write `name: chart-index`, the
// server submits `name: release/oberth/chart-index`.
func scopeSynchronization(workflow *wfv1.Workflow, request Request) {
	scope := func(name string) string {
		return string(request.Trigger) + "/" + request.Repo + "/" + name
	}
	rewrite := func(sync *wfv1.Synchronization) {
		if sync == nil {
			return
		}
		for i := range sync.Mutexes {
			if sync.Mutexes[i] != nil && sync.Mutexes[i].Name != "" {
				sync.Mutexes[i].Name = scope(sync.Mutexes[i].Name)
			}
		}
	}
	rewrite(workflow.Spec.Synchronization)
	for i := range workflow.Spec.Templates {
		rewrite(workflow.Spec.Templates[i].Synchronization)
	}
}

// applyServerBounds caps the document's own deadline and pins retention. A
// document can ask for less than the administrator ceiling but never more.
func applyServerBounds(workflow *wfv1.Workflow, config Config) {
	ceiling := int64(config.WorkflowTimeout / time.Second)
	if workflow.Spec.ActiveDeadlineSeconds == nil || *workflow.Spec.ActiveDeadlineSeconds > ceiling {
		bounded := ceiling
		workflow.Spec.ActiveDeadlineSeconds = &bounded
	}
	ttl := config.TTLSeconds
	workflow.Spec.TTLStrategy = &wfv1.TTLStrategy{SecondsAfterCompletion: &ttl}
	// No PodGC: owner-reference collection removes step Pods together with the
	// TTL-deleted Workflow, preserving pod logs for the full retention window.
	// PodGCOnWorkflowCompletion deleted pods at completion, before the TTL
	// collected the Workflow — a late Wait or restart lost the pods that
	// replayStepLog uses as its only log source.
	workflow.Spec.PodGC = nil
}

// The step-Pod security baseline. These are forced server-side on every
// container the pipeline runs, and Admit refuses the repository-authored
// fields that could weaken them, so the two halves cannot disagree.
//
// The Argo-path security baseline runs as UID 0 with RunAsNonRoot false,
// drops all Linux capabilities, sets a read-only root filesystem with a
// writable /tmp emptyDir, and applies RuntimeDefault seccomp. Tightening to
// a non-root UID (65534) is a separate, riskier follow-up that needs real
// step execution validated against the toolchain and the PVC's ownership.
//
// The container-level baseline is deliberately STRICTER than the Go path:
// ReadOnlyRootFilesystem is true rather than false. That is safe here only
// because this engine also injects a writable emptyDir at /tmp below --
// os.TempDir() is where the Go toolchain writes, so a read-only root without
// it would break every build.
const (
	stepTmpVolumeName = "oberth-tmp"
	stepTmpMountPath  = "/tmp"
)

// applyServerSecurity forces the Pod and container security baseline.
//
// Without this the Argo path set no securityContext at all, and Admit only
// refused an explicitly-set privileged:true -- so a document declaring
// seccompProfile: Unconfined, or runAsUser: 0 on a cluster whose default is
// non-root, ran exactly as it asked on the same node as the server.
func applyServerSecurity(workflow *wfv1.Workflow) {
	runAsNonRoot := false
	uid, gid := int64(0), int64(0)
	workflow.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    &uid,
		RunAsGroup:   &gid,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	// A writable scratch mount, so the read-only root filesystem below cannot
	// break the toolchain. Server-owned: Admit refuses a repository volume of
	// any kind other than emptyDir, and this name is reserved by collision.
	workflow.Spec.Volumes = append(workflow.Spec.Volumes, corev1.Volume{
		Name:         stepTmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	mount := corev1.VolumeMount{Name: stepTmpVolumeName, MountPath: stepTmpMountPath}
	for index := range workflow.Spec.Templates {
		applyTemplateSecurity(&workflow.Spec.Templates[index], mount, 0)
	}
}

func applyTemplateSecurity(template *wfv1.Template, mount corev1.VolumeMount, depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	// Template-level Pod security is cleared: the workflow-level baseline is
	// the only one, so a template cannot present a different posture.
	template.SecurityContext = nil
	harden := func(container *corev1.Container) {
		noEscalation, readOnlyRoot := false, true
		container.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
		filtered := container.VolumeMounts[:0:0]
		for _, existing := range container.VolumeMounts {
			if existing.Name == stepTmpVolumeName || existing.MountPath == stepTmpMountPath {
				continue
			}
			filtered = append(filtered, existing)
		}
		container.VolumeMounts = append(filtered, mount)
	}
	if template.Container != nil {
		harden(template.Container)
	}
	if template.Script != nil {
		harden(&template.Script.Container)
	}
	if template.ContainerSet != nil {
		for index := range template.ContainerSet.Containers {
			harden(&template.ContainerSet.Containers[index].Container)
		}
	}
	for index := range template.InitContainers {
		harden(&template.InitContainers[index].Container)
	}
	for index := range template.Sidecars {
		harden(&template.Sidecars[index].Container)
	}
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			applyTemplateSecurity(template.Steps[group].Steps[step].Inline, mount, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			applyTemplateSecurity(template.DAG.Tasks[task].Inline, mount, depth+1)
		}
	}
}

// SourceVolumeName and SourceMountPath are where every pipeline container
// finds the pushed revision. They are server-owned names: a document that
// declares a volume of the same name is refused by the duplicate check
// Kubernetes itself applies, and could not have mounted a claim anyway.
const (
	SourceVolumeName = "oberth-source"
	SourceMountPath  = "/work/src"

	// VaultCAMountPath and VaultCACertPath are where a release-tier container
	// finds the OpenBao trust anchor. They are outside SourceMountPath so the
	// repository's tree stays exactly what was pushed, and server-owned for
	// the same reason the source mount is.
	VaultCAMountPath = "/run/oberth/vault-ca"
	VaultCACertPath  = VaultCAMountPath + "/" + vaultCACertFile

	// OberthBinMountPath is where a credentialed container finds the Oberth
	// server binary, delivered from the source claim. The binary enables
	// `oberth secretstore exec` as the native credential chain replacement.
	OberthBinMountPath = "/run/oberth/bin"
	// OberthBinPath is the full path to the oberth binary inside the mount.
	OberthBinPath = OberthBinMountPath + "/oberth"

	// SecretsMountPath is where the memory-backed emptyDir for materialised
	// secrets is mounted. `oberth secretstore exec` writes fetched credentials
	// here, and pipeline steps read them via OBERTH_SECRETSTORE_DIR.
	SecretsMountPath = "/run/oberth-secrets" // #nosec G101 -- a mount path, not a credential.

	// SecretsVolumeName is the server-owned name for the memory-backed secrets
	// emptyDir volume.
	// #nosec G101 -- a volume name, not a credential.
	SecretsVolumeName = "oberth-secrets"

	// secretsSizeLimit bounds the memory-backed emptyDir. Release credentials
	// are small (a few KB each); 16 MiB is generous and limits the memory
	// pressure a single run can impose on the node.
	secretsSizeLimit = "16Mi"

	// ReleaseTokenVolumeName and ReleaseTokenMountPath deliver the projected
	// ServiceAccount token a release-tier step logs in to OpenBao with. The
	// path is the one Kubernetes has always used, because that is what every
	// Vault client, including the envconsul configuration in a repository,
	// reads by default.
	// #nosec G101 -- a volume name and a mount path. The token they deliver is
	// minted by the kubelet inside the Pod; no credential exists here.
	ReleaseTokenVolumeName = "oberth-release-token"
	// #nosec G101 -- the conventional Kubernetes mount path, not a credential.
	ReleaseTokenMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	// releaseTokenExpirationSeconds is the kubelet's rotation window, and the
	// Kubernetes minimum. The token is presented once, at the start of a step,
	// to obtain a Vault token with its own shorter TTL.
	releaseTokenExpirationSeconds = int64(600)

	// CacheVolumeName and CacheMountPath are where every pipeline container
	// finds the persistent cross-run build cache. Server-owned, like the source
	// mount and for the same reason: the repository never names this volume,
	// never chooses the node directory behind it, and cannot opt out of it.
	//
	// It sits beside /work/src rather than under /tmp because /tmp is the
	// emptyDir that makes the read-only root filesystem survivable, and
	// applyServerSecurity reserves that exact path.
	CacheVolumeName = "oberth-cache"
	CacheMountPath  = "/work/cache"

	// ModuleCachePath and BuildCachePath are the two directories inside the
	// cache mount, advertised to steps as GOMODCACHE and GOCACHE.
	//
	// These two and no others. Go documents its build cache as safe for
	// concurrent use by multiple processes on one machine over a local
	// filesystem -- they coordinate with operating system file locks, may
	// duplicate effort, and will not corrupt the cache -- and the module cache
	// takes a per-version lock for the same reason. A partial or evicted entry
	// degrades to a cache miss and is recomputed, never to a wrong build.
	//
	// Nothing else is shared. Tool binaries in particular stay on the run's own
	// ephemeral volume: `go install` publishes into GOBIN with a plain
	// truncating write, so two concurrent runs installing the same pin could
	// briefly expose a half-written executable to a later step's exec. A warm
	// build cache makes that install a relink of a few seconds anyway, so
	// sharing it would trade a real concurrency hazard for almost nothing.
	ModuleCachePath = CacheMountPath + "/gomod"
	BuildCachePath  = CacheMountPath + "/gobuild"

	// maxRepoCacheNameLength bounds the readable half of a cache directory name.
	// The digest that follows it is what actually keeps two repositories apart.
	maxRepoCacheNameLength = 48

	// WorkspaceVolumeName is the conventional volumeClaimTemplate name an
	// Argo-authored pipeline declares for its cross-step workspace. When a
	// pipeline declares a VCT with this name, the server injects standard
	// Go-toolchain mount paths (tools, caches) and the environment that
	// references them, so each template does not have to repeat the same block.
	//
	// Step-specific settings (GOBIN, CGO_ENABLED, GOARCH, GOMEMLIMIT) stay in
	// the YAML; they are step configuration, not infrastructure.
	WorkspaceVolumeName = "work"

	// Standard mount paths from the workspace VCT. Each is a subPath of the
	// "work" claim, shared across every step in the same Argo Workflow.
	//
	// The tools, trivy-cache, and release paths live under /tmp, which
	// applyServerSecurity already mounts as a writable emptyDir. A subPath
	// mount from the VCT takes precedence at its specific directory while the
	// emptyDir backs everything else under /tmp.
	WorkspaceToolsMountPath      = "/tmp/oberth-tools"
	WorkspaceTrivyCacheMountPath = "/tmp/oberth-trivy"
	WorkspaceReleaseMountPath    = "/tmp/oberth-release"

	// Per-run fallback cache paths inside the workspace VCT. When the
	// administrator configures a persistent cross-run cache (--ci-cache-root /
	// --release-cache-root), injectRunEnvironment overrides GOMODCACHE and
	// GOCACHE to point at the persistent mount, and these VCT sub-paths go
	// unused but remain mounted (harmlessly). When no persistent cache is
	// configured, these keep the Go module and build caches shared between the
	// steps of one run.
	WorkspaceGomodFallbackPath   = "/cache/gomod"
	WorkspaceGobuildFallbackPath = "/cache/gobuild"
)

// cacheRootFor selects the trust tier's cache root.
//
// It is the same shape as identityFor, deliberately: the trigger picks the
// ServiceAccount and the trigger picks the cache, so there is one answer to
// "what tier is this run" rather than two that could drift apart. An unknown
// trigger gets no cache, which fails closed.
func (config Config) cacheRootFor(trigger periapsis.Trigger) string {
	switch trigger {
	case periapsis.TriggerCI:
		return strings.TrimSpace(config.CICacheRoot)
	case periapsis.TriggerRelease:
		return strings.TrimSpace(config.ReleaseCacheRoot)
	default:
		return ""
	}
}

// repoCacheSegment derives the single directory name a repository's cache lives
// under, inside its tier's root.
//
// The repository name arrives from a push, so it is never used verbatim in a
// node path. Only lowercase alphanumerics, '-' and '_' survive; '.' is
// deliberately not in that set, which makes "." and ".." structurally
// unreachable rather than filtered for. The trailing digest is taken over the
// original name, so two repositories that sanitise or truncate to the same
// readable prefix still land in different directories.
func repoCacheSegment(repo string) string {
	digest := sha256.Sum256([]byte(repo))
	var safe strings.Builder
	for _, character := range strings.ToLower(repo) {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			safe.WriteRune(character)
		default:
			safe.WriteByte('-')
		}
	}
	readable := strings.Trim(safe.String(), "-_")
	if len(readable) > maxRepoCacheNameLength {
		readable = strings.Trim(readable[:maxRepoCacheNameLength], "-_")
	}
	if readable == "" {
		readable = "repo"
	}
	return readable + "-" + hex.EncodeToString(digest[:])[:12]
}

// cacheVolumeAndMount builds this run's cache volume, or reports that the tier
// has no cache configured.
//
// DirectoryOrCreate is what makes the first run for a repository work without
// anything having provisioned its directory in advance; the tier root itself is
// created and owned by the chart's prepare-caches init container.
func cacheVolumeAndMount(config Config, request Request) (corev1.Volume, corev1.VolumeMount, bool) {
	root := config.cacheRootFor(request.Trigger)
	if root == "" {
		return corev1.Volume{}, corev1.VolumeMount{}, false
	}
	hostPathType := corev1.HostPathDirectoryOrCreate
	volume := corev1.Volume{
		Name: CacheVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: path.Join(root, repoCacheSegment(request.Repo)),
				Type: &hostPathType,
			},
		},
	}
	// Writable on purpose, and the only writable server-owned mount a step gets.
	return volume, corev1.VolumeMount{Name: CacheVolumeName, MountPath: CacheMountPath}, true
}

// injectServerVolumes mounts everything the server owns into the pipeline's
// containers: the tier's persistent build cache, the run's immutable checkout,
// and -- on the release tier, only in templates whose command is envconsul --
// the projected ServiceAccount token the credential chain logs in with. When
// the pipeline declares a "work" volumeClaimTemplate, the standard
// Go-toolchain workspace paths (tools, per-run fallback caches, trivy cache,
// release artifacts) are also injected into every container.
//
// The Go path solves the checkout problem by mounting the same PVC with the
// run's subPath into its single Job container. Doing it server-side here keeps
// the property that made that safe: the repository never names the claim, never
// chooses the subPath, and never gets it writable.
//
// Common mounts (cache, source, vault CA, workspace) go to every container via
// injectTemplateVolumeMounts. The release token mount is scoped to credentialed
// templates by a separate pass via injectCredentialVolumeMounts; see the inline
// comment at the injection site for why the passes must be separate.
func injectServerVolumes(workflow *wfv1.Workflow, config Config, request Request, credentialed bool) {
	var mounts []corev1.VolumeMount
	if volume, mount, ok := cacheVolumeAndMount(config, request); ok {
		workflow.Spec.Volumes = append(workflow.Spec.Volumes, volume)
		mounts = append(mounts, mount)
	}
	if claim := strings.TrimSpace(request.SourceVolume.ClaimName); claim != "" {
		workflow.Spec.Volumes = append(workflow.Spec.Volumes, corev1.Volume{
			Name: SourceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      SourceVolumeName,
			MountPath: SourceMountPath,
			SubPath:   request.SourceVolume.SubPath,
			ReadOnly:  true,
		})
		if credentialed && request.vaultCADelivered() {
			// The anchor rides the same claim as a second subPath, so it needs
			// no volume of its own and is collected with the run's storage.
			mounts = append(mounts, corev1.VolumeMount{
				Name:      SourceVolumeName,
				MountPath: VaultCAMountPath,
				SubPath:   request.SourceVolume.VaultCASubPath,
				ReadOnly:  true,
			})
		}
		if credentialed && request.binaryDelivered() {
			// The server binary rides the same claim as another subPath,
			// enabling `oberth secretstore exec` in pipeline containers.
			mounts = append(mounts, corev1.VolumeMount{
				Name:      SourceVolumeName,
				MountPath: OberthBinMountPath,
				SubPath:   request.SourceVolume.BinarySubPath,
				ReadOnly:  true,
			})
		}
	}
	// Workspace mounts: standard Go-toolchain subPaths from the pipeline's own
	// "work" volumeClaimTemplate. The VCT itself is repository-declared; these
	// mounts are server-injected so each template does not repeat the block.
	// injectTemplateVolumeMounts strips existing YAML mounts at the same paths
	// (via its mountPath collision rule), so a template that still declares one
	// gets the server's version -- making this additive and idempotent.
	if hasWorkspaceVCT(workflow) {
		mounts = append(mounts, workspaceVolumeMounts(request.Trigger)...)
	}
	// The projected token volume is declared at the workflow level for every
	// credentialed Workflow (release, or CI with declared secret-store paths),
	// but the mount is scoped to credentialed templates -- those whose
	// container command is envconsul. Non-credentialed templates (setup, lint,
	// test, scan, chart-test, build, package-chart) carry no login token and
	// cannot attempt a Vault login regardless of what their ServiceAccount is
	// bound to.
	//
	// The volume and the mount are split across two injection passes. The first
	// pass (injectTemplateVolumeMounts) strips repository mounts that collide
	// with any server-owned volume name -- including ReleaseTokenVolumeName --
	// regardless of which mounts it is injecting. A second call to the same
	// function with only the credential mount would strip the cache and source
	// mounts the first call added, because the name-collision check is
	// hardcoded. injectCredentialVolumeMounts avoids that: it appends the
	// mount to qualifying templates and strips only colliding paths.
	var credentialMounts []corev1.VolumeMount
	if credentialed {
		workflow.Spec.Volumes = append(workflow.Spec.Volumes, releaseTokenVolume())
		credentialMounts = append(credentialMounts, releaseTokenMount())
		// Memory-backed emptyDir for materialised secrets. `oberth secretstore
		// exec` writes fetched credentials here; pipeline steps read them via
		// OBERTH_SECRETSTORE_DIR. The volume is server-owned so component repos
		// do not declare it themselves, and the sizeLimit bounds node memory.
		sizeLimit := resource.MustParse(secretsSizeLimit)
		workflow.Spec.Volumes = append(workflow.Spec.Volumes, corev1.Volume{
			Name: SecretsVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &sizeLimit,
				},
			},
		})
		credentialMounts = append(credentialMounts, corev1.VolumeMount{
			Name:      SecretsVolumeName,
			MountPath: SecretsMountPath,
		})
	}
	if len(mounts) == 0 && len(credentialMounts) == 0 {
		return
	}
	for index := range workflow.Spec.Templates {
		injectTemplateVolumeMounts(&workflow.Spec.Templates[index], mounts, 0)
	}
	for index := range workflow.Spec.Templates {
		injectCredentialVolumeMounts(&workflow.Spec.Templates[index], credentialMounts, 0)
	}
}

// releaseTokenMount and releaseTokenVolume declare the release tier's login
// credential explicitly instead of relying on the kubelet's automount.
//
// Found by running it. Oberth asks for a token by setting the Workflow's
// automountServiceAccountToken to true, and Argo writes that field onto the Pod
// only when it is FALSE (workflow/controller/workflowpod.go, setupServiceAccount
// in v4.1.0). A request for true therefore leaves the Pod field unset, and
// Kubernetes' ServiceAccount admission plugin then falls through to the
// ServiceAccount object -- which this chart's own argo-identities.yaml sets to
// automountServiceAccountToken: false, correctly, for all three identities.
//
// The two halves are each defensible and together they produce a release Pod
// with no token at all: every credentialed step in .oberth/release.yaml failed
// its Vault login on a missing token file, with nothing in the error naming
// either decision. Asking the chart to stop disabling automount would fix it by
// making the release tier's credential depend on a default that looks like
// something a careful administrator should turn off.
//
// So the server declares the token, on the tier that needs one, at the path
// every Kubernetes Vault client reads. It is bounded (ten minutes, rotated by
// the kubelet) rather than the legacy non-expiring mount, it exists because
// Oberth put it there rather than because nobody removed it, and a container
// that already mounts this path is skipped by the admission plugin, so nothing
// can double-mount it.
func releaseTokenMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      ReleaseTokenVolumeName,
		MountPath: ReleaseTokenMountPath,
		ReadOnly:  true,
	}
}

func releaseTokenVolume() corev1.Volume {
	expiration := releaseTokenExpirationSeconds
	return corev1.Volume{
		Name: ReleaseTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						// No audience: OpenBao's Kubernetes auth validates the
						// token through TokenReview against the API server's own
						// audience, which is what an automounted token carries.
						// Naming one here would require every role to name the
						// same one, and would fail closed at login with an error
						// about the role rather than about the audience.
						ExpirationSeconds: &expiration,
						Path:              "token",
					},
				}},
			},
		},
	}
}

// vaultCADelivered reports whether this run's seeding actually wrote the trust
// anchor into the source claim. The subpath being set is the indication: only a
// run whose claim holds the file may advertise its path.
func (request Request) vaultCADelivered() bool {
	return strings.TrimSpace(request.SourceVolume.VaultCASubPath) != ""
}

// binaryDelivered reports whether this run's seeding wrote the server binary
// into the source claim.
func (request Request) binaryDelivered() bool {
	return strings.TrimSpace(request.SourceVolume.BinarySubPath) != ""
}

func injectTemplateVolumeMounts(template *wfv1.Template, injected []corev1.VolumeMount, depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	// One pass over all server-owned mounts, not one pass each: a per-mount
	// filter keyed on the volume name would strip the mount the previous pass
	// had just added, and the source tree would silently vanish the moment a
	// second mount of the same claim existed.
	reserved := make(map[string]struct{}, len(injected))
	for _, mount := range injected {
		reserved[mount.MountPath] = struct{}{}
	}
	attach := func(mounts []corev1.VolumeMount) []corev1.VolumeMount {
		filtered := mounts[:0:0]
		for _, existing := range mounts {
			// Server-owned volume names and server-owned paths both win: a
			// document must not be able to point either one somewhere else.
			if existing.Name == SourceVolumeName ||
				existing.Name == ReleaseTokenVolumeName ||
				existing.Name == CacheVolumeName ||
				existing.Name == SecretsVolumeName {
				continue
			}
			if _, taken := reserved[existing.MountPath]; taken {
				continue
			}
			filtered = append(filtered, existing)
		}
		return append(filtered, injected...)
	}
	if template.Container != nil {
		template.Container.VolumeMounts = attach(template.Container.VolumeMounts)
	}
	if template.Script != nil {
		template.Script.VolumeMounts = attach(template.Script.VolumeMounts)
	}
	if template.ContainerSet != nil {
		for index := range template.ContainerSet.Containers {
			template.ContainerSet.Containers[index].VolumeMounts =
				attach(template.ContainerSet.Containers[index].VolumeMounts)
		}
	}
	for index := range template.InitContainers {
		template.InitContainers[index].VolumeMounts = attach(template.InitContainers[index].VolumeMounts)
	}
	for index := range template.Sidecars {
		template.Sidecars[index].VolumeMounts = attach(template.Sidecars[index].VolumeMounts)
	}
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			injectTemplateVolumeMounts(template.Steps[group].Steps[step].Inline, injected, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			injectTemplateVolumeMounts(template.DAG.Tasks[task].Inline, injected, depth+1)
		}
	}
}

// templateUsesEnvconsul reports whether a template's main container command is
// the envconsul binary, which identifies it as a credentialed template that
// needs the release-tier login token. The check matches the same condition the
// existing tests use (oberth_pipeline_test.go, TestOberthReleasePipeline
// WrapsCredentialedStepsWithEnvconsul).
func templateUsesEnvconsul(template *wfv1.Template) bool {
	isEnvconsul := func(command []string) bool {
		return len(command) > 0 &&
			(strings.HasSuffix(command[0], "/envconsul") || command[0] == "envconsul")
	}
	if template.Container != nil && isEnvconsul(template.Container.Command) {
		return true
	}
	if template.Script != nil && isEnvconsul(template.Script.Command) {
		return true
	}
	return false
}

// templateUsesOberthSecretstore reports whether a template's main container
// command is `oberth secretstore exec` or `oberth secretstore materialize`.
// These are the native credential chain replacements that handle vault fetch,
// file materialisation, env stripping, and redaction in a single binary.
func templateUsesOberthSecretstore(template *wfv1.Template) bool {
	isOberthSecretstore := func(command, args []string) bool {
		if len(command) == 0 {
			return false
		}
		// command[0] is the binary path; args carries the subcommands and flags.
		if !strings.HasSuffix(command[0], "/oberth") && command[0] != "oberth" &&
			command[0] != OberthBinPath {
			return false
		}
		// The subcommand words may be in command[1:] or in args[0:].
		rest := append(command[1:], args...)
		if len(rest) < 2 {
			return false
		}
		return rest[0] == "secretstore" && (rest[1] == "exec" || rest[1] == "materialize")
	}
	if template.Container != nil &&
		isOberthSecretstore(template.Container.Command, template.Container.Args) {
		return true
	}
	if template.Script != nil &&
		isOberthSecretstore(template.Script.Command, template.Script.Args) {
		return true
	}
	return false
}

// templateUsesCredentialChain reports whether a template uses any recognised
// credential chain: envconsul (legacy) or oberth secretstore exec/materialize
// (native).
func templateUsesCredentialChain(template *wfv1.Template) bool {
	return templateUsesEnvconsul(template) || templateUsesOberthSecretstore(template)
}

// extractExecPaths returns the --path arguments from an `oberth secretstore exec`
// invocation in a template's container command+args.
func extractExecPaths(template *wfv1.Template) []string {
	extract := func(command, args []string) []string {
		// Combine command and args into one slice for scanning.
		all := append(command, args...)
		var paths []string
		for i := 0; i < len(all); i++ {
			arg := all[i]
			if arg == "--" {
				break
			}
			if rest, found := strings.CutPrefix(arg, "--path="); found {
				paths = append(paths, rest)
				continue
			}
			if rest, found := strings.CutPrefix(arg, "-path="); found {
				paths = append(paths, rest)
				continue
			}
			if arg == "--path" || arg == "-path" {
				if i+1 < len(all) {
					i++
					paths = append(paths, all[i])
				}
			}
		}
		return paths
	}
	if template.Container != nil {
		return extract(template.Container.Command, template.Container.Args)
	}
	if template.Script != nil {
		return extract(template.Script.Command, template.Script.Args)
	}
	return nil
}

// injectCredentialVolumeMounts adds the release-tier login token to templates
// whose container command is envconsul -- the credential chain -- and to no
// others. Non-credentialed templates in a release Workflow (setup, lint, test,
// scan, chart-test, build, package-chart) have no use for the projected
// ServiceAccount token and must not mount it.
//
// This is a separate pass from injectTemplateVolumeMounts because that
// function strips repository mounts matching ANY server-owned volume name
// (SourceVolumeName, ReleaseTokenVolumeName, CacheVolumeName) on every call.
// A second call with only the credential mount would strip the source and
// cache mounts the first call added. This function avoids that by stripping
// only colliding mount paths, not volume names: the first pass already
// ensured no repository-declared name collision survived.
func injectCredentialVolumeMounts(template *wfv1.Template, mounts []corev1.VolumeMount, depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth || len(mounts) == 0 {
		return
	}
	// Mount the credential token ONLY on the exact Container or Script whose
	// command is a recognised credential chain (envconsul or oberth secretstore
	// exec/materialize). Init containers and sidecars are repository-selected
	// images that do not need the store credential and must not receive it: an
	// init container running before the main step, or a sidecar sharing the Pod
	// network, could exfiltrate the projected token.
	if templateUsesCredentialChain(template) {
		reserved := make(map[string]struct{}, len(mounts))
		for _, mount := range mounts {
			reserved[mount.MountPath] = struct{}{}
		}
		attach := func(existing []corev1.VolumeMount) []corev1.VolumeMount {
			filtered := existing[:0:0]
			for _, m := range existing {
				if _, taken := reserved[m.MountPath]; taken {
					continue
				}
				filtered = append(filtered, m)
			}
			return append(filtered, mounts...)
		}
		// Attach to the main container only when it runs a credential
		// chain (envconsul or oberth secretstore exec/materialize).
		if template.Container != nil {
			template.Container.VolumeMounts = attach(template.Container.VolumeMounts)
		}
		if template.Script != nil {
			template.Script.VolumeMounts = attach(template.Script.VolumeMounts)
		}
		// ContainerSet, init containers, and sidecars never receive the
		// credential mount regardless of their command.
	}
	// Recurse into inline templates regardless: an inline child may use
	// envconsul even when its parent template does not.
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			injectCredentialVolumeMounts(template.Steps[group].Steps[step].Inline, mounts, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			injectCredentialVolumeMounts(template.DAG.Tasks[task].Inline, mounts, depth+1)
		}
	}
}

// hasWorkspaceVCT reports whether the pipeline declares a volumeClaimTemplate
// named "work". When present, the server injects the standard Go-toolchain
// environment and workspace mount paths; when absent, the pipeline is assumed to
// manage its own workspace and the injection is skipped.
func hasWorkspaceVCT(workflow *wfv1.Workflow) bool {
	for _, claim := range workflow.Spec.VolumeClaimTemplates {
		if claim.Name == WorkspaceVolumeName {
			return true
		}
	}
	return false
}

// workspaceEnvironment returns the common Go-toolchain environment that every
// container in a workspace-equipped pipeline receives. These are the values
// that were previously copy-pasted into every template's env block.
//
// Step-specific overrides (GOBIN, CGO_ENABLED, GOARCH, GOMEMLIMIT) stay in the
// YAML and survive this injection: overrideEnvironment strips the server's value
// for a name collision, so a step that declares GOBIN keeps its own value while
// the workspace provides the baseline it does not need to repeat.
//
// GOMODCACHE and GOCACHE point at the per-run VCT fallback paths. When the
// administrator configures a persistent cache, injectRunEnvironment (which runs
// after this) overrides them with the persistent mount paths.
func workspaceEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PATH", Value: WorkspaceToolsMountPath + "/bin:/usr/local/bin:/usr/bin:/bin"},
		{Name: "OBERTH_TOOLS_DIR", Value: WorkspaceToolsMountPath},
		{Name: "HOME", Value: "/tmp"},
		{Name: "GOPATH", Value: WorkspaceToolsMountPath + "/gopath"},
		{Name: "GOTOOLCHAIN", Value: "local"},
		{Name: "GOWORK", Value: "off"},
		{Name: "GOENV", Value: "off"},
		{Name: "GOMODCACHE", Value: WorkspaceGomodFallbackPath},
		{Name: "GOCACHE", Value: WorkspaceGobuildFallbackPath},
		{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
		{Name: "KUBEBUILDER_ASSETS", Value: ""},
		{Name: "TRIVY_CACHE_DIR", Value: WorkspaceTrivyCacheMountPath},
		{Name: "GOLANGCI_LINT_CACHE", Value: WorkspaceToolsMountPath + "/cache/golangci-lint"},
	}
}

// workspaceVolumeMounts returns the standard subPath mounts from the "work"
// VCT. On the release tier an additional mount for release artifacts is
// included; CI runs do not need it.
func workspaceVolumeMounts(trigger periapsis.Trigger) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: WorkspaceVolumeName, MountPath: WorkspaceToolsMountPath, SubPath: "tools"},
		{Name: WorkspaceVolumeName, MountPath: WorkspaceGomodFallbackPath, SubPath: "gomod"},
		{Name: WorkspaceVolumeName, MountPath: WorkspaceGobuildFallbackPath, SubPath: "gobuild"},
		{Name: WorkspaceVolumeName, MountPath: WorkspaceTrivyCacheMountPath, SubPath: "trivy-cache"},
	}
	if trigger == periapsis.TriggerRelease {
		mounts = append(mounts, corev1.VolumeMount{
			Name: WorkspaceVolumeName, MountPath: WorkspaceReleaseMountPath, SubPath: "release",
		})
	}
	return mounts
}

// injectWorkspaceEnvironment gives every container the standard Go-toolchain
// baseline when the pipeline declares a "work" VCT. It runs before
// injectRunEnvironment so the persistent-cache overrides (GOMODCACHE, GOCACHE)
// and the run-identity variables take precedence over these defaults.
func injectWorkspaceEnvironment(workflow *wfv1.Workflow) {
	if !hasWorkspaceVCT(workflow) {
		return
	}
	environment := workspaceEnvironment()
	for index := range workflow.Spec.Templates {
		injectTemplateEnvironment(&workflow.Spec.Templates[index], environment, 0)
	}
}

// injectRunEnvironment gives every container the same run identity variables
// the Go path's runner receives, plus the Vault coordinates on any
// credentialed tier. Repository-set values are preserved: a name collision
// resolves in favour of the server's value, because these describe the run,
// not the step.
func injectRunEnvironment(workflow *wfv1.Workflow, config Config, request Request, credentialed bool) {
	environment := []corev1.EnvVar{
		{Name: "OBERTH_REPO", Value: request.Repo},
		{Name: "OBERTH_REF", Value: request.Ref},
		{Name: "OBERTH_SHA", Value: request.SHA},
		{Name: "OBERTH_TRIGGER", Value: string(request.Trigger)},
		{Name: "OBERTH_RUN_ID", Value: request.RunID},
	}
	if config.cacheRootFor(request.Trigger) != "" {
		// The server states where the cache is, exactly as it states where the
		// checkout and the Vault anchor are. A repository that sets GOMODCACHE or
		// GOCACHE itself loses to these by this function's name-collision rule,
		// which is what stops a document from pointing the compiler at a
		// directory the server did not mount -- or at the other tier's.
		//
		// Go creates both directories on first use, so nothing has to pre-make
		// them inside the mount.
		environment = append(environment,
			corev1.EnvVar{Name: "GOMODCACHE", Value: ModuleCachePath},
			corev1.EnvVar{Name: "GOCACHE", Value: BuildCachePath},
			corev1.EnvVar{Name: "OBERTH_CACHE_DIR", Value: CacheMountPath},
		)
	}
	if request.Trigger == periapsis.TriggerRelease {
		// release.sh reads these from the environment (not from Argo workflow
		// parameters). They are the release-tier counterparts of OBERTH_REF
		// and OBERTH_SHA: the ref is the tag (e.g. "refs/tags/v0.12.11") and
		// the SHA is the exact commit the tag points to.
		environment = append(environment,
			corev1.EnvVar{Name: "OBERTH_RELEASE_TAG", Value: request.Ref},
			corev1.EnvVar{Name: "OBERTH_RELEASE_SHA", Value: request.SHA},
		)
	}
	if credentialed {
		// The role matches the tier identityFor selected: the ci-secrets role
		// on the CI trigger, the credentialed role on the release trigger.
		// Advertising the release role to a CI pod would only name a login
		// its ServiceAccount cannot complete, and every failure would blame
		// the wrong tier.
		environment = append(environment,
			corev1.EnvVar{Name: "VAULT_ADDR", Value: config.VaultAddress},
			corev1.EnvVar{Name: "OBERTH_VAULT_ROLE", Value: config.vaultRoleFor(request.Trigger)},
		)
		if request.vaultCADelivered() {
			// The path, never the bytes. envconsul's Vault client reads
			// VAULT_CACERT when its config declares no ssl block, so the
			// repository's envconsul.hcl states how to authenticate and the
			// server states what to trust -- consistent with VAULT_ADDR.
			environment = append(environment, corev1.EnvVar{Name: "VAULT_CACERT", Value: VaultCACertPath})
		}
	}
	for index := range workflow.Spec.Templates {
		injectTemplateEnvironment(&workflow.Spec.Templates[index], environment, 0)
	}
}

func injectTemplateEnvironment(template *wfv1.Template, environment []corev1.EnvVar, depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	if template.Container != nil {
		template.Container.Env = overrideEnvironment(template.Container.Env, environment)
	}
	if template.Script != nil {
		template.Script.Env = overrideEnvironment(template.Script.Env, environment)
	}
	if template.ContainerSet != nil {
		for index := range template.ContainerSet.Containers {
			template.ContainerSet.Containers[index].Env =
				overrideEnvironment(template.ContainerSet.Containers[index].Env, environment)
		}
	}
	for index := range template.InitContainers {
		template.InitContainers[index].Env = overrideEnvironment(template.InitContainers[index].Env, environment)
	}
	for index := range template.Sidecars {
		template.Sidecars[index].Env = overrideEnvironment(template.Sidecars[index].Env, environment)
	}
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			injectTemplateEnvironment(template.Steps[group].Steps[step].Inline, environment, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			injectTemplateEnvironment(template.DAG.Tasks[task].Inline, environment, depth+1)
		}
	}
}

func overrideEnvironment(existing []corev1.EnvVar, injected []corev1.EnvVar) []corev1.EnvVar {
	reserved := make(map[string]struct{}, len(injected))
	for _, variable := range injected {
		reserved[variable.Name] = struct{}{}
	}
	result := make([]corev1.EnvVar, 0, len(existing)+len(injected))
	for _, variable := range existing {
		if _, taken := reserved[variable.Name]; taken {
			continue
		}
		result = append(result, variable)
	}
	return append(result, injected...)
}

func (request Request) validate() error {
	var problems []error
	if strings.TrimSpace(request.RunID) == "" {
		problems = append(problems, errors.New("argojob: run ID is required"))
	}
	if messages := k8svalidation.IsDNS1123Subdomain(request.Name); len(messages) != 0 || len(request.Name) > 63 {
		problems = append(problems, fmt.Errorf("argojob: Workflow name %q must be a DNS-1123 subdomain no longer than 63 characters", request.Name))
	}
	if strings.TrimSpace(request.Repo) == "" {
		problems = append(problems, errors.New("argojob: repository is required"))
	}
	if strings.TrimSpace(request.Ref) == "" || strings.ContainsAny(request.Ref, "\x00\r\n") {
		problems = append(problems, errors.New("argojob: ref is required and must not contain control characters"))
	}
	if !fullSHA(request.SHA) {
		problems = append(problems, errors.New("argojob: SHA must be a lowercase 40-character object ID"))
	}
	if !request.Trigger.Valid() {
		problems = append(problems, fmt.Errorf("argojob: trigger %q is invalid", request.Trigger))
	}
	if len(request.Source) == 0 {
		problems = append(problems, errors.New("argojob: pipeline document is required"))
	}
	return errors.Join(problems...)
}

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func labelValue(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	trimmed := strings.Trim(result.String(), "-_.")
	if len(trimmed) > k8scontent.LabelValueMaxLength {
		trimmed = trimmed[:k8scontent.LabelValueMaxLength]
	}
	return strings.TrimRight(trimmed, "-_.")
}

// WorkflowMeta is the object identity a submitted Workflow must keep for the
// engine to accept it as its own on recovery.
func WorkflowMeta(workflow *wfv1.Workflow) (runID string, identity string, ok bool) {
	if workflow == nil {
		return "", "", false
	}
	runID = workflow.Annotations[runIDAnnotation]
	identity = workflow.Annotations[identityAnnotation]
	return runID, identity, runID != ""
}
