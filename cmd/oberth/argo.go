package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/goproxy"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

// buildArgoEngine wires the Argo execution engine from serve flags.
//
// It is reached only when --argo-namespace is set. Every trust decision the
// engine makes comes from these flags, never from a repository document, so
// this function is the complete administrator surface of the Argo path.
func buildArgoEngine(
	options serveOptions,
	restConfig *rest.Config,
	kube kubernetes.Interface,
	auditor service.Auditor,
	secretAccess app.SecretAccessLoader,
	fragments app.FragmentLoader,
	artifactStore app.ArtifactStore,
	artifactLimit int64,
	artifactBudget int64,
	perRepoIdentities map[string]argojob.PerRepoIdentityConfig,
	identityStore *argojob.IdentityStore,
) (*app.ArgoJobs, error) {
	argoClient, err := wfclientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("configure Argo Workflow client: %w", err)
	}
	// Read once at startup, not per run: the anchor a release verifies its
	// store against is a deployment decision, and re-reading it mid-flight
	// would let a file change between two steps of one release.
	vaultCACertPEM, err := readArgoVaultCACert(options.argoVaultCACert)
	if err != nil {
		return nil, err
	}
	goProxyCACertPEM, err := readArgoVaultCACert(options.argoGoProxyCA)
	if err != nil {
		return nil, fmt.Errorf("serve: read --argo-goproxy-ca: %w", err)
	}
	wifConfig, err := readArgoReleaseWIFConfig(options.argoReleaseWIFConfig)
	if err != nil {
		return nil, err
	}
	config := argojob.Config{
		ReleaseWIF:                 wifConfig,
		NonrootProfile:             options.argoControllerProfile,
		Namespace:                  options.argoNamespace,
		PipelineServiceAccount:     options.argoPipelineAccount,
		CredentialedServiceAccount: options.argoCredentialedAccount,
		CISecretsServiceAccount:    options.argoCISecretsAccount,
		ExecutorServiceAccount:     options.argoExecutorAccount,
		RunnerImagePrefixes:        splitRunnerImagePrefixes(options.runnerImagePrefixes),
		VaultAddress:               options.argoVaultAddress,
		VaultCredentialedRole:      options.argoVaultCredentialedRole,
		VaultCISecretsRole:         options.argoVaultCISecretsRole,
		VaultCACertPEM:             vaultCACertPEM,
		GoProxyURL:                 options.argoGoProxyURL,
		GoProxyModulePrefix:        options.argoGoProxyNamespace.ModulePrefix,
		GoProxyCACertPEM:           goProxyCACertPEM,
		// Pipeline containers read this revision's checkout from a claim the
		// server creates and fills in the pipeline namespace, because a Pod
		// cannot mount the server's own claim across a namespace boundary.
		SourceStorageClass: options.argoSourceStorageClass,
		// The node-local module and build caches, split by trust tier. These are
		// the same two roots the chart already creates and owns through its
		// prepare-caches init container; the Argo engine is what finally reads
		// them, so a branch build stops recompiling its toolchain every push.
		CICacheRoot:      options.ciCacheRoot,
		ReleaseCacheRoot: options.releaseCacheRoot,
		WorkflowTimeout:  options.argoWorkflowTimeout,
		// #nosec G115 -- validateServeOptions bounds argoWorkflowTTL to a positive int32.
		TTLSeconds:          int32(options.argoWorkflowTTL),
		PerRepoIdentities:   perRepoIdentities,
		PerRepoCIIdentities: buildPerRepoCIIdentities(perRepoIdentities),
		KVMEnabled:          options.vmKVMEnabled,
	}
	controller, err := argojob.NewController(
		argoClient.ArgoprojV1alpha1().Workflows(options.argoNamespace), kube, config)
	if err != nil {
		return nil, err
	}
	seeder := argojob.NewSourceSeeder(kube, newArgoExecStreamer(restConfig, kube), config)
	if seeder == nil {
		return nil, errors.New("configure Argo source seeding: the in-cluster Kubernetes client is required")
	}
	jobs, err := app.NewArgoJobs(controller.WithSourceSeeder(seeder), config, auditor, secretAccess, fragments)
	if err != nil {
		return nil, err
	}
	if artifactStore != nil {
		jobs.SetArtifacts(seeder, artifactStore, artifactLimit, artifactBudget)
	}
	if identityStore != nil {
		jobs.SetIdentityStore(identityStore)
	}
	return jobs, nil
}

// validateArgoServeOptions fails a misconfigured Argo engine at startup rather
// than at the first release.
//
// The checks are deliberately strict about identity. Every one of them, if
// wrong, produces a cluster where the tier separation looks configured but is
// not: a shared ServiceAccount would let a branch push satisfy the Vault role
// bound to the release tier, and the pipeline namespace sharing Oberth's own
// would put pipeline identities beside server ones.
func validateArgoServeOptions(options serveOptions) error {
	if options.argoNamespace == "" {
		if options.argoReleaseWIFConfig != "" {
			return errors.New("serve: --argo-release-wif-config requires --argo-namespace")
		}
		return nil
	}
	if options.argoNamespace == options.namespace {
		return errors.New("serve: --argo-namespace must differ from --namespace; pipeline identities must not share a namespace with the server")
	}
	if options.argoWorkflowTimeout <= 0 || options.argoWorkflowTTL <= 0 || options.argoWorkflowTTL > 1<<31-1 {
		return errors.New("serve: --argo-workflow-timeout and --argo-workflow-ttl must be positive")
	}
	if options.argoVaultCredentialedRole != "" && options.argoVaultAddress == "" {
		return errors.New("serve: --argo-vault-credentialed-role needs --argo-vault-address")
	}
	if options.argoVaultCISecretsRole != "" && options.argoVaultAddress == "" {
		return errors.New("serve: --argo-vault-ci-secrets-role needs --argo-vault-address")
	}
	if options.argoVaultCACert != "" &&
		(!filepath.IsAbs(options.argoVaultCACert) || filepath.Clean(options.argoVaultCACert) != options.argoVaultCACert) {
		return errors.New("serve: --argo-vault-ca-cert must be a clean absolute path")
	}
	// Go module proxy flags must come as a complete set or be absent entirely.
	goProxyHasListener := strings.TrimSpace(options.argoGoProxyListen) != ""
	goProxyHasCert := strings.TrimSpace(options.argoGoProxyCert) != ""
	goProxyHasKey := strings.TrimSpace(options.argoGoProxyKey) != ""
	goProxyHasCA := strings.TrimSpace(options.argoGoProxyCA) != ""
	goProxyHasURL := strings.TrimSpace(options.argoGoProxyURL) != ""
	goProxyHasNamespace := options.argoGoProxyNamespace != (goproxy.NamespaceConfig{})
	goProxyAny := goProxyHasListener || goProxyHasCert || goProxyHasKey || goProxyHasCA || goProxyHasURL || goProxyHasNamespace
	goProxyAll := goProxyHasListener && goProxyHasCert && goProxyHasKey && goProxyHasCA && goProxyHasURL
	if goProxyAny && !goProxyAll {
		return errors.New("serve: --argo-goproxy-listen, --argo-goproxy-cert, --argo-goproxy-key, --argo-goproxy-ca, and --argo-goproxy-url must all be set or all be empty")
	}
	if goProxyAll {
		if err := options.argoGoProxyNamespace.Validate(); err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}
	if goProxyHasURL && !strings.HasPrefix(options.argoGoProxyURL, "https://") {
		return errors.New("serve: --argo-goproxy-url must be https://")
	}
	for _, pathFlag := range []struct{ name, value string }{
		{"--argo-goproxy-cert", options.argoGoProxyCert},
		{"--argo-goproxy-key", options.argoGoProxyKey},
		{"--argo-goproxy-ca", options.argoGoProxyCA},
	} {
		if pathFlag.value != "" && (!filepath.IsAbs(pathFlag.value) || filepath.Clean(pathFlag.value) != pathFlag.value) {
			return fmt.Errorf("serve: %s must be a clean absolute path", pathFlag.name)
		}
	}
	// Reading the file here as well as in buildArgoEngine is deliberate: a
	// trust anchor that is missing, unreadable, or not a certificate should
	// fail the process at startup, not the first release tag of the quarter.
	vaultCACertPEM, err := readArgoVaultCACert(options.argoVaultCACert)
	if err != nil {
		return err
	}
	// argojob.Config.Validate carries the rest: DNS-1123 names, all four
	// ServiceAccounts distinct, an https:// Vault address, and a well-formed
	// runner image allowlist. Running it here means a bad flag fails startup.
	// Note: goproxy CA is not read here — the file only exists on the pod, not
	// on the host where parseServeOptions runs for the chart-contract test.
	// buildArgoEngine reads it at runtime.
	wifConfig, err := readArgoReleaseWIFConfig(options.argoReleaseWIFConfig)
	if err != nil {
		return err
	}
	return argojob.Config{
		ReleaseWIF:                 wifConfig,
		NonrootProfile:             options.argoControllerProfile,
		Namespace:                  options.argoNamespace,
		PipelineServiceAccount:     options.argoPipelineAccount,
		CredentialedServiceAccount: options.argoCredentialedAccount,
		CISecretsServiceAccount:    options.argoCISecretsAccount,
		ExecutorServiceAccount:     options.argoExecutorAccount,
		RunnerImagePrefixes:        splitRunnerImagePrefixes(options.runnerImagePrefixes),
		VaultAddress:               options.argoVaultAddress,
		VaultCredentialedRole:      options.argoVaultCredentialedRole,
		VaultCISecretsRole:         options.argoVaultCISecretsRole,
		VaultCACertPEM:             vaultCACertPEM,
		// GoProxyURL and GoProxyCACertPEM are validated in buildArgoEngine, not
		// here: the CA PEM file only exists on the pod's Secret volume, and the
		// Config.Validate pair check (URL ↔ CA) cannot run without both fields.
		// The flag-syntax checks above (HTTPS, clean paths, all-or-none) catch
		// the errors reachable from flag text alone.
		// Carried here too so a cache root that is relative, unclean, or shared
		// between the two tiers fails the process at startup rather than
		// silently mounting one tier's writable state into the other's steps.
		CICacheRoot:      options.ciCacheRoot,
		ReleaseCacheRoot: options.releaseCacheRoot,
		KVMEnabled:       options.vmKVMEnabled,
	}.Validate()
}

// readArgoVaultCACert loads the trust anchor release-tier pipeline containers
// verify the store against.
//
// It is a plain file read rather than an x509.CertPool because the bytes
// themselves travel: the server does not use this anchor, it delivers it, and
// what it delivers has to be the PEM an envconsul in another Pod can parse.
// argojob.Config.Validate is what decides the bytes are a usable anchor.
func readArgoVaultCACert(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	pemBytes, err := os.ReadFile(path) // #nosec G304 -- an administrator flag, validated as a clean absolute path.
	if err != nil {
		return "", fmt.Errorf("serve: read --argo-vault-ca-cert: %w", err)
	}
	return string(pemBytes), nil
}

// newArgoExecStreamer delivers source trees over the Kubernetes exec
// subresource into a running claim-init container.
func newArgoExecStreamer(restConfig *rest.Config, kube kubernetes.Interface) argojob.ExecStreamer {
	return func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		request := kube.CoreV1().RESTClient().Post().
			Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container,
				Command:   command,
				Stdin:     stdin != nil,
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)
		websocketExecutor, err := remotecommand.NewWebSocketExecutor(restConfig, http.MethodGet, request.URL().String())
		if err != nil {
			return fmt.Errorf("prepare exec transport: %w", err)
		}
		spdyExecutor, err := remotecommand.NewSPDYExecutor(restConfig, http.MethodPost, request.URL())
		if err != nil {
			return fmt.Errorf("prepare exec fallback transport: %w", err)
		}
		executor, err := remotecommand.NewFallbackExecutor(websocketExecutor, spdyExecutor, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
		if err != nil {
			return fmt.Errorf("prepare exec executor: %w", err)
		}
		return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
}

type artifactStoreAdapter struct {
	store *artifacts.Store
	// scanPatterns is the redaction gate set (#208) every extraction runs
	// under; the persisting call site sets it to DefaultScanPatterns.
	scanPatterns []string
}

func (adapter artifactStoreAdapter) Extract(runID string, stream io.Reader, limit int64) error {
	_, err := adapter.store.Extract(runID, stream, limit, adapter.scanPatterns)
	return err
}

func (adapter artifactStoreAdapter) Evict(budget int64) ([]string, error) {
	return adapter.store.Evict(budget)
}

// perRepoStore is the narrow interface the per-repo identity producer needs.
// *store.Store satisfies it directly.
type perRepoStore interface {
	ListRepositories(context.Context) ([]model.Repository, error)
	ListUpstreams(context.Context) ([]model.Upstream, error)
	ActiveSecretGrants(ctx context.Context, repoID int64) (map[string]map[string]bool, error)
}

// buildPerRepoIdentities reads the repo registry and approval table, and
// builds the per-repo identity map that scopes each credentialed repository
// to its own Vault policy at the ServiceAccount level. Only repositories
// with at least one active secret grant get a per-repo identity; repos
// without grants continue using the shared credentialed identity.
//
// This is the serve-path producer for issue #246 phase 2. The installer
// path has its own producer that reads from the ConfigMap and the live pod.
func buildPerRepoIdentities(ctx context.Context, db perRepoStore) (map[string]argojob.PerRepoIdentityConfig, error) {
	repos, err := db.ListRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repositories for per-repo identities: %w", err)
	}
	upstreams, err := db.ListUpstreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list upstreams for per-repo identities: %w", err)
	}

	upstreamByID := make(map[int64]model.Upstream, len(upstreams))
	for _, u := range upstreams {
		upstreamByID[u.ID] = u
	}

	result := make(map[string]argojob.PerRepoIdentityConfig)
	for _, repo := range repos {
		upstream, ok := upstreamByID[repo.UpstreamID]
		if !ok {
			continue
		}
		org := upstream.Org()
		if org == "" {
			continue
		}

		grants, err := db.ActiveSecretGrants(ctx, repo.ID)
		if err != nil {
			return nil, fmt.Errorf("load grants for %s: %w", repo.Name, err)
		}
		if len(grants) == 0 {
			continue
		}

		key := upstream.Name + "/" + org + "/" + repo.Name
		saName := installer.PerRepoName(upstream.Name, org, repo.Name)
		result[key] = argojob.PerRepoIdentityConfig{
			ServiceAccountName: saName,
		}
	}

	return result, nil
}

// buildPerRepoCIIdentities derives CI-tier per-repo identities from the
// release-tier per-repo identity map. Every repo with a release per-repo
// identity gets a corresponding CI per-repo identity. The CI identity uses
// PerRepoCIName to produce a structurally distinct ServiceAccount name, and
// its Vault policy is grant-free — scoped to the repo's own upstream namespace
// only, closing the org-union gap in the shared ci-secrets policy (issue #433).
func buildPerRepoCIIdentities(releaseIdentities map[string]argojob.PerRepoIdentityConfig) map[string]argojob.PerRepoIdentityConfig {
	if len(releaseIdentities) == 0 {
		return nil
	}
	result := make(map[string]argojob.PerRepoIdentityConfig, len(releaseIdentities))
	for key := range releaseIdentities {
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			continue
		}
		ciName := installer.PerRepoCIName(parts[0], parts[1], parts[2])
		result[key] = argojob.PerRepoIdentityConfig{
			ServiceAccountName: ciName,
		}
	}
	return result
}

// buildPerStepIdentities reads the repo registry and approval table, and
// builds the per-step identity map that scopes each named-step grant to its
// own Vault policy at the ServiceAccount level. Only (repo, step) combinations
// with named-step grants (step != "*") get a per-step identity; wildcard grants
// keep the per-repo identity (backward compatible). Issue #623, finding 1.
func buildPerStepIdentities(ctx context.Context, db perRepoStore) (map[string]argojob.PerRepoIdentityConfig, error) {
	repos, err := db.ListRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repositories for per-step identities: %w", err)
	}
	upstreams, err := db.ListUpstreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list upstreams for per-step identities: %w", err)
	}

	upstreamByID := make(map[int64]model.Upstream, len(upstreams))
	for _, u := range upstreams {
		upstreamByID[u.ID] = u
	}

	result := make(map[string]argojob.PerRepoIdentityConfig)
	for _, repo := range repos {
		upstream, ok := upstreamByID[repo.UpstreamID]
		if !ok {
			continue
		}
		org := upstream.Org()
		if org == "" {
			continue
		}

		grants, err := db.ActiveSecretGrants(ctx, repo.ID)
		if err != nil {
			return nil, fmt.Errorf("load grants for %s: %w", repo.Name, err)
		}

		// grants is map[step]map[secret]bool. Scan for named steps.
		for step := range grants {
			if step == "*" {
				continue
			}
			// Named-step grant: create a per-step identity.
			key := upstream.Name + "/" + org + "/" + repo.Name + "/" + step
			saName := installer.PerStepName(upstream.Name, org, repo.Name, step)
			result[key] = argojob.PerRepoIdentityConfig{
				ServiceAccountName: saName,
			}
		}
	}

	return result, nil
}
