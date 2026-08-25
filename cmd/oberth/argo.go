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
	config := argojob.Config{
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
		TTLSeconds: int32(options.argoWorkflowTTL),
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
	return app.NewArgoJobs(controller.WithSourceSeeder(seeder), config, auditor, secretAccess)
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
	return argojob.Config{
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
		// Carried here too so a cache root that is relative, unclean, or shared
		// between the two tiers fails the process at startup rather than
		// silently mounting one tier's writable state into the other's steps.
		CICacheRoot:      options.ciCacheRoot,
		ReleaseCacheRoot: options.releaseCacheRoot,
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
