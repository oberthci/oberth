package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	oberth "github.com/oberthci/oberth"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/pkg/periapsis"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The secretstore command group splits secret-store work by where the
// authority for it lives:
//
//   - `setup` guidance and the embedded setup script belong NEXT TO the store,
//     where the administrator's own CLI session already holds admin authority.
//     This binary has no code path that accepts a store admin token.
//   - `verify` belongs IN THIS POD, because the thing under test is the
//     workload identity itself: it logs in with the pod's ServiceAccount token
//     through the production fetch code path and proves the whole trust chain
//     (auth mount, role binding, TokenReview, read policy, TLS) end to end
//     without any credential beyond the identity the server already has.
const defaultServeProcCmdline = "/proc/1/cmdline"

const maximumServeCmdlineBytes = 1 << 20

func runSecretStore(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: secretstore setup|verify|sync|exec|materialize", errUsage)
	}
	switch arguments[0] {
	case "setup":
		return runSecretStoreSetup(arguments[1:], output)
	case "verify":
		return runSecretStoreVerify(ctx, arguments[1:], output, defaultServeProcCmdline)
	case "sync":
		return runSecretStoreSync(ctx, arguments[1:], output)
	case "bootstrap-tls":
		return runSecretStoreBootstrapTLS(arguments[1:], output)
	case "exec":
		return runSecretStoreExec(ctx, arguments[1:], output, os.Stderr)
	case "materialize":
		return runSecretStoreMaterialize(ctx, arguments[1:], output, os.Stderr)
	default:
		return fmt.Errorf("%w: unknown secretstore command %q", errUsage, arguments[0])
	}
}

// runSecretStoreBootstrapTLS is an installer-internal command executed only
// in the short-lived, credential-free bootstrap Pod. It persists private
// material directly on OpenBao's PVC and emits exactly the public CA PEM.
func runSecretStoreBootstrapTLS(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("secretstore bootstrap-tls", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	outputDirectory := flags.String("output-dir", "", "absolute OpenBao PVC directory for TLS material")
	namespace := flags.String("namespace", "", "OpenBao Kubernetes namespace")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 || *outputDirectory == "" || *namespace == "" {
		return fmt.Errorf("%w: secretstore bootstrap-tls requires --output-dir and --namespace", errUsage)
	}
	caPEM, err := secretstore.BootstrapOpenBaoTLS(*outputDirectory, *namespace)
	if err != nil {
		return err
	}
	_, err = output.Write(caPEM)
	return err
}

func runSecretStoreSetup(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("secretstore setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	printScript := flags.Bool("print-script", false, "write the embedded setup script to stdout")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: secretstore setup accepts flags only", errUsage)
	}
	if *printScript {
		_, err := output.Write(oberth.SetupSecretStoreScript)
		return err
	}
	digest := sha256.Sum256(oberth.SetupSecretStoreScript)
	_, err := fmt.Fprintf(output, `Store-side setup runs NEXT TO your OpenBao (or Vault), never in this pod:
this binary deliberately has no code path that accepts a store admin token.

1. Extract the setup script from this signed image (or use the identical
   scripts/setup-secretstore.sh from the Oberth repository):

     kubectl exec -n oberth deploy/oberth -- \
       oberth secretstore setup --print-script > setup-secretstore.sh
     sha256sum setup-secretstore.sh   # %s

2. Inspect it, then run it on the machine where your bao/vault CLI is already
   authenticated. It enables Kubernetes auth, writes a minimal read-only
   KV policy plus exact Transit encrypt/decrypt grants, creates one
   non-exportable trusted-plan key, binds a role to the Oberth ServiceAccount,
   and prints the exact helm values for step 3.

3. helm upgrade --install with the printed secretstore.* values.

4. Prove the trust chain end to end from inside this pod:

     kubectl exec -n oberth deploy/oberth -- oberth secretstore verify
`, hex.EncodeToString(digest[:]))
	return err
}

// secretStoreVerifyConfig carries exactly the serve-side secret store settings
// the verifier needs; every field mirrors one --secretstore-* serve flag.
type secretStoreVerifyConfig struct {
	address      string
	authMount    string
	role         string
	caCertPath   string
	saTokenPath  string
	kvMount      string
	insecureHTTP bool
	paths        []string
}

// releaseTierVerifyConfig carries exactly the Argo credentialed-tier secret
// store settings parsed from the serve command line; every field mirrors one
// --argo-* serve flag.
type releaseTierVerifyConfig struct {
	vaultAddress          string
	vaultCACertPath       string
	vaultCredentialedRole string
	credentialedAccount   string
	pipelineNamespace     string
}

// kubeClientFactory builds a Kubernetes client; the production implementation
// uses rest.InClusterConfig, and tests inject a fake.
type kubeClientFactory func() (kubernetes.Interface, error)

// stringSliceFlag implements flag.Value for a repeatable string flag.
type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return strings.Join(*f, ", ") }
func (f *stringSliceFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func defaultKubeClient() (kubernetes.Interface, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w (run inside the Oberth pod)", err)
	}
	restConfig.Timeout = 10 * time.Second
	return kubernetes.NewForConfig(restConfig)
}

func runSecretStoreVerify(ctx context.Context, arguments []string, output io.Writer, procCmdline string) error {
	flags := flag.NewFlagSet("secretstore verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var explicit secretStoreVerifyConfig
	flags.StringVar(&explicit.address, "address", "", "OpenBao API base URL (default: discovered from the running serve process)")
	flags.StringVar(&explicit.authMount, "k8s-auth-mount", "", "OpenBao Kubernetes auth mount path")
	flags.StringVar(&explicit.role, "role", "", "OpenBao Kubernetes auth role")
	flags.StringVar(&explicit.caCertPath, "ca-cert", "", "optional PEM file pinning the OpenBao TLS trust anchors")
	flags.StringVar(&explicit.saTokenPath, "sa-token", "", "optional ServiceAccount token path override")
	flags.StringVar(&explicit.kvMount, "kv-mount", "", "KV v2 mount backing the virtual oberth/upstream/ namespace (default: discovered from serve, else oberth)")
	flags.BoolVar(&explicit.insecureHTTP, "insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address")
	releaseTier := flags.Bool("release-tier", false, "verify the release-tier OpenBao trust chain (Argo pipeline namespace, release ServiceAccount, --argo-vault-* flags)")
	repoFlag := flags.String("repo", "", "verify a per-repo identity (upstream/org/repo); with --release-tier, overrides the shared SA")
	tierFlag := flags.String("tier", "release", "which per-repo tier to verify: release or ci (only with --repo or --release-tier)")
	timeout := flags.Duration("timeout", 45*time.Second, "overall verification deadline")
	keys := flags.Bool("keys", false, "verify or list KV field names; use with --expect to assert")
	var expects stringSliceFlag
	flags.Var(&expects, "expect", "expected fields: <path-base>/<field>[,<field>,...] (repeatable, implies -keys)")
	var pathFlags stringSliceFlag
	flags.Var(&pathFlags, "path", "KV path to verify (repeatable; alternative to positional paths)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *tierFlag != "release" && *tierFlag != "ci" {
		return fmt.Errorf("%w: --tier must be \"release\" or \"ci\"", errUsage)
	}
	if *releaseTier {
		if *keys || len(expects) > 0 {
			return fmt.Errorf("%w: -keys and --expect are not yet supported with --release-tier", errUsage)
		}
		var mergedPaths []string
		mergedPaths = append(mergedPaths, pathFlags...)
		mergedPaths = append(mergedPaths, flags.Args()...)
		return runReleaseTierVerify(ctx, explicit, mergedPaths, *repoFlag, *tierFlag, *timeout, output, procCmdline, defaultKubeClient)
	}
	options := explicit
	if options.address == "" {
		if options.role != "" || options.authMount != "" || options.caCertPath != "" || options.saTokenPath != "" || options.kvMount != "" || options.insecureHTTP {
			return fmt.Errorf("%w: explicit secretstore verify flags require --address", errUsage)
		}
		discovered, err := secretStoreConfigFromServeCmdline(procCmdline)
		if err != nil {
			return fmt.Errorf("discover the running server's secret store configuration: %w (run inside the Oberth pod, or pass --address and --role explicitly)", err)
		}
		options = discovered
	}
	if options.address == "" {
		return errors.New("this server has no secret store configured: serve runs without --secretstore-address; set secretstore.enabled=true in the chart first")
	}
	if strings.TrimSpace(options.role) == "" {
		return fmt.Errorf("%w: --address requires --role", errUsage)
	}
	// --path flags and positional arguments override the discovered allowlist
	// so a candidate path can be proven BEFORE an administrator allowlists it.
	var paths []string
	paths = append(paths, pathFlags...)
	paths = append(paths, flags.Args()...)
	if len(paths) == 0 {
		paths = options.paths
	}
	if len(paths) == 0 {
		return errors.New("nothing to verify: the server allowlists no --secretstore-path values; pass one or more KV API paths (e.g. oberth/data/r2-upload) or scoped paths (e.g. oberth/upstream/<org>/<repo>/<secret>) as arguments")
	}
	kvMount := options.kvMount
	if kvMount == "" {
		kvMount = secretstore.DefaultKVMount
	}
	// Virtual oberth/upstream/ arguments are canonicalized to the KV v2 API
	// path exactly as release admission does, so the administrator can prove
	// a scoped secret with the very string a repository declares.
	declaredByFetch := make(map[string]string, len(paths))
	for index, path := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(path)
		if err != nil {
			return err
		}
		if upstreamScoped {
			canonical := scoped.FetchPath(kvMount)
			declaredByFetch[canonical] = path
			paths[index] = canonical
		}
	}
	var caPEM []byte
	if options.caCertPath != "" {
		body, err := readBoundedFile(options.caCertPath, 4<<20)
		if err != nil {
			return fmt.Errorf("read secret store CA certificate: %w", err)
		}
		caPEM = body
	}
	client, err := secretstore.New(secretstore.Config{
		Address:                 options.address,
		AllowInsecureHTTP:       options.insecureHTTP,
		AuthMountPath:           options.authMount,
		Role:                    options.role,
		CACertPEM:               caPEM,
		ServiceAccountTokenPath: options.saTokenPath,
		Timeout:                 *timeout,
	})
	if err != nil {
		return fmt.Errorf("configure secret store verification: %w", err)
	}
	mount := options.authMount
	if mount == "" {
		mount = secretstore.DefaultAuthMountPath
	}
	if _, err := fmt.Fprintf(output, "secret store verify: address=%s mount=%s role=%s paths=%d\n",
		options.address, mount, options.role, len(paths)); err != nil {
		return err
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fetched, err := client.FetchKV(deadlineCtx, paths)
	if err != nil {
		_, _ = fmt.Fprint(output, secretStoreVerifyHints)
		return fmt.Errorf("secret store verify failed: %w", err)
	}
	// The fetch pulled real secret values through the production code path;
	// the verifier reports only key counts and wipes every value before it
	// returns. Values are never printed, logged, or written anywhere.
	defer zeroFetchedSecrets(fetched)
	keysMode := *keys || len(expects) > 0
	for _, path := range paths {
		label := path
		if declared, scoped := declaredByFetch[path]; scoped {
			label = declared + " -> " + path
		}
		if keysMode {
			fieldNames := sortedFieldNames(fetched[path])
			if _, err := fmt.Fprintf(output, "  ok %s [%s]\n", label, strings.Join(fieldNames, ", ")); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(output, "  ok %s (%d keys)\n", label, len(fetched[path])); err != nil {
				return err
			}
		}
	}
	if len(expects) > 0 {
		if err := verifyExpectedKeys(output, fetched, paths, []string(expects)); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "secret store verify: OK — field names match expectations")
		return err
	}
	if keysMode {
		_, err = fmt.Fprintln(output, "secret store verify: OK — field names listed above (pass --expect to assert)")
		return err
	}
	_, err = fmt.Fprintln(output, "secret store verify: OK — ServiceAccount login, TokenReview, read policy, and TLS all verified")
	return err
}

// runReleaseTierVerify proves the release-tier OpenBao trust chain end to end.
// The server tier (secretstore verify without --release-tier) verifies the
// server's own leg: its own namespace, ServiceAccount, and secretstore.caCert.
// The release tier runs in a completely independent namespace with a different
// ServiceAccount and a separate CA anchor; nothing automated exercises this
// path until a real release tag runs a credentialed step.
//
// The approach:
//  1. Discover --argo-vault-*, --argo-credentialed-serviceaccount, and --argo-namespace
//     from the serve process command line.
//  2. Request a short-lived token for the credentialed ServiceAccount via the
//     Kubernetes TokenRequest API (the server's SA must have create permission
//     on serviceaccounts/token in the pipeline namespace).
//  3. Attempt a real Vault Kubernetes-auth login with that token, against
//     --argo-vault-address, using --argo-vault-ca-cert as the CA anchor.
//  4. Fetch at least one path from the server's --secretstore-path allowlist
//     to prove read access.
func runReleaseTierVerify(
	ctx context.Context,
	explicit secretStoreVerifyConfig,
	positionalPaths []string,
	repo string,
	tier string,
	timeout time.Duration,
	output io.Writer,
	procCmdline string,
	newKube kubeClientFactory,
) error {
	// --release-tier is mutually exclusive with explicit server-tier flags.
	if explicit.address != "" || explicit.role != "" || explicit.authMount != "" ||
		explicit.caCertPath != "" || explicit.saTokenPath != "" || explicit.kvMount != "" || explicit.insecureHTTP {
		return fmt.Errorf("%w: --release-tier is mutually exclusive with explicit server-tier flags (--address, --role, --ca-cert, etc.)", errUsage)
	}

	releaseConfig, err := releaseTierConfigFromServeCmdline(procCmdline)
	if err != nil {
		return fmt.Errorf("discover release-tier configuration: %w (run inside the Oberth pod)", err)
	}
	if releaseConfig.vaultAddress == "" {
		return errors.New("release tier is not configured: serve runs without --argo-vault-address; set argo.vault.address in the chart")
	}
	if releaseConfig.pipelineNamespace == "" {
		return errors.New("release tier is not configured: serve runs without --argo-namespace")
	}

	// When --repo is given, verify the per-repo identity instead of the shared one.
	// The per-repo SA and role name are derived deterministically from the
	// upstream/org/repo triple, matching the runtime's PerRepoName / PerRepoCIName.
	if repo != "" {
		return runPerRepoTierVerify(ctx, releaseConfig, positionalPaths, repo, tier, timeout, output, procCmdline, newKube)
	}

	// Shared SA verification (existing behavior, backward compatible).
	if releaseConfig.vaultCredentialedRole == "" {
		return errors.New("release tier is not configured: serve runs without --argo-vault-credentialed-role; set argo.vault.credentialedRole in the chart")
	}
	if releaseConfig.credentialedAccount == "" {
		return errors.New("release tier is not configured: serve runs without --argo-credentialed-serviceaccount")
	}

	// The allowlisted paths are shared: the server declares them in
	// --secretstore-path and both tiers read the same KV entries.
	serverConfig, err := secretStoreConfigFromServeCmdline(procCmdline)
	if err != nil {
		return fmt.Errorf("discover server secret store configuration for path allowlist: %w", err)
	}
	paths := positionalPaths
	if len(paths) == 0 {
		paths = serverConfig.paths
	}
	if len(paths) == 0 {
		return errors.New("nothing to verify: the server allowlists no --secretstore-path values; pass one or more KV API paths as positional arguments")
	}
	kvMount := serverConfig.kvMount
	if kvMount == "" {
		kvMount = secretstore.DefaultKVMount
	}
	declaredByFetch := make(map[string]string, len(paths))
	for index, path := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(path)
		if err != nil {
			return err
		}
		if upstreamScoped {
			canonical := scoped.FetchPath(kvMount)
			declaredByFetch[canonical] = path
			paths[index] = canonical
		}
	}

	// Obtain a short-lived token for the release ServiceAccount via the
	// Kubernetes TokenRequest API. This proves the server's own RBAC grants
	// are correct before touching the store.
	kube, err := newKube()
	if err != nil {
		return err
	}
	expirationSeconds := int64(900) // 15 minutes; Kubernetes rejects durations below 600s
	tokenRequest, err := kube.CoreV1().ServiceAccounts(releaseConfig.pipelineNamespace).CreateToken(
		ctx,
		releaseConfig.credentialedAccount,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: &expirationSeconds,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("request token for release ServiceAccount %s/%s: %w (does the server's ServiceAccount have serviceaccounts/token create permission in the pipeline namespace?)",
			releaseConfig.pipelineNamespace, releaseConfig.credentialedAccount, err)
	}
	token := tokenRequest.Status.Token
	if token == "" {
		return errors.New("TokenRequest returned an empty token for the release ServiceAccount")
	}

	// Write the token to a temporary file so the secretstore client can
	// read it through its existing file-based path. The file is removed
	// immediately after verification completes.
	tokenFile, err := os.CreateTemp("", "oberth-release-verify-*.token")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	tokenPath := tokenFile.Name()
	defer func() { _ = os.Remove(tokenPath) }()
	if _, err := tokenFile.WriteString(token); err != nil {
		_ = tokenFile.Close()
		return fmt.Errorf("write temporary token file: %w", err)
	}
	if err := tokenFile.Close(); err != nil {
		return fmt.Errorf("close temporary token file: %w", err)
	}

	var caPEM []byte
	if releaseConfig.vaultCACertPath != "" {
		body, err := readBoundedFile(releaseConfig.vaultCACertPath, 4<<20)
		if err != nil {
			return fmt.Errorf("read release-tier Vault CA certificate (%s): %w", releaseConfig.vaultCACertPath, err)
		}
		caPEM = body
	}

	client, err := secretstore.New(secretstore.Config{
		Address:                 releaseConfig.vaultAddress,
		AuthMountPath:           secretstore.DefaultAuthMountPath,
		Role:                    releaseConfig.vaultCredentialedRole,
		CACertPEM:               caPEM,
		ServiceAccountTokenPath: tokenPath,
		Timeout:                 timeout,
	})
	if err != nil {
		return fmt.Errorf("configure release-tier verification: %w", err)
	}

	if _, err := fmt.Fprintf(output, "release-tier verify: address=%s role=%s sa=%s/%s paths=%d\n",
		releaseConfig.vaultAddress, releaseConfig.vaultCredentialedRole,
		releaseConfig.pipelineNamespace, releaseConfig.credentialedAccount, len(paths)); err != nil {
		return err
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetched, err := client.FetchKV(deadlineCtx, paths)
	if err != nil {
		_, _ = fmt.Fprint(output, releaseTierVerifyHints)
		return fmt.Errorf("release-tier verify failed: %w", err)
	}
	defer zeroFetchedSecrets(fetched)
	for _, path := range paths {
		label := path
		if declared, scoped := declaredByFetch[path]; scoped {
			label = declared + " -> " + path
		}
		if _, err := fmt.Fprintf(output, "  ok %s (%d keys)\n", label, len(fetched[path])); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, "release-tier verify: OK — TokenRequest, release SA login, Vault auth, read policy, and TLS all verified end to end")
	return err
}

// runPerRepoTierVerify verifies a per-repo identity (release or CI tier) by
// resolving the per-repo SA and role via PerRepoName / PerRepoCIName,
// TokenRequesting that SA, and attempting a Vault login + fetch.
//
// This is the fix for issue #428: the shared SA verification answered a
// question no real release asks, because every repo that actually fetches
// release secrets has its own per-repo identity.
func runPerRepoTierVerify(
	ctx context.Context,
	releaseConfig releaseTierVerifyConfig,
	positionalPaths []string,
	repo string,
	tier string,
	timeout time.Duration,
	output io.Writer,
	procCmdline string,
	newKube kubeClientFactory,
) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 3 {
		return fmt.Errorf("%w: --repo must be upstream/org/repo (got %q)", errUsage, repo)
	}
	upstream, org, repoName := parts[0], parts[1], parts[2]

	// Derive the per-repo SA name and role name using the same derivation
	// the runtime uses, so the verification exercises the exact identity
	// a real run would bind to.
	var saName string
	var tierLabel string
	switch tier {
	case "release":
		saName = installer.PerRepoName(upstream, org, repoName)
		tierLabel = "release"
	case "ci":
		saName = installer.PerRepoCIName(upstream, org, repoName)
		tierLabel = "ci"
	default:
		return fmt.Errorf("%w: --tier must be \"release\" or \"ci\"", errUsage)
	}
	// Per-repo SA name == role name by convention.
	roleName := saName

	serverConfig, err := secretStoreConfigFromServeCmdline(procCmdline)
	if err != nil {
		return fmt.Errorf("discover server secret store configuration: %w", err)
	}
	kvMount := serverConfig.kvMount
	if kvMount == "" {
		kvMount = secretstore.DefaultKVMount
	}

	// For per-repo verification, the paths come from positional args or,
	// if none are given, we construct a synthetic test path from the repo's
	// upstream namespace that the per-repo policy should grant.
	paths := positionalPaths
	if len(paths) == 0 {
		paths = serverConfig.paths
	}
	declaredByFetch := make(map[string]string, len(paths))
	for index, path := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(path)
		if err != nil {
			return err
		}
		if upstreamScoped {
			canonical := scoped.FetchPath(kvMount)
			declaredByFetch[canonical] = path
			paths[index] = canonical
		}
	}
	if len(paths) == 0 {
		return fmt.Errorf("nothing to verify: pass one or more KV API paths to test the %s per-repo identity for %s", tierLabel, repo)
	}

	kube, err := newKube()
	if err != nil {
		return err
	}
	expirationSeconds := int64(900)
	tokenRequest, err := kube.CoreV1().ServiceAccounts(releaseConfig.pipelineNamespace).CreateToken(
		ctx,
		saName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: &expirationSeconds,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("request token for %s per-repo ServiceAccount %s/%s: %w "+
			"(does the SA exist? run \"oberth install --install-secretstore --upgrade\" to create it)",
			tierLabel, releaseConfig.pipelineNamespace, saName, err)
	}
	token := tokenRequest.Status.Token
	if token == "" {
		return fmt.Errorf("TokenRequest returned an empty token for %s per-repo ServiceAccount %s", tierLabel, saName)
	}

	tokenFile, err := os.CreateTemp("", "oberth-perrepo-verify-*.token")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	tokenPath := tokenFile.Name()
	defer func() { _ = os.Remove(tokenPath) }()
	if _, err := tokenFile.WriteString(token); err != nil {
		_ = tokenFile.Close()
		return fmt.Errorf("write temporary token file: %w", err)
	}
	if err := tokenFile.Close(); err != nil {
		return fmt.Errorf("close temporary token file: %w", err)
	}

	var caPEM []byte
	if releaseConfig.vaultCACertPath != "" {
		body, err := readBoundedFile(releaseConfig.vaultCACertPath, 4<<20)
		if err != nil {
			return fmt.Errorf("read release-tier Vault CA certificate (%s): %w", releaseConfig.vaultCACertPath, err)
		}
		caPEM = body
	}

	client, err := secretstore.New(secretstore.Config{
		Address:                 releaseConfig.vaultAddress,
		AuthMountPath:           secretstore.DefaultAuthMountPath,
		Role:                    roleName,
		CACertPEM:               caPEM,
		ServiceAccountTokenPath: tokenPath,
		Timeout:                 timeout,
	})
	if err != nil {
		return fmt.Errorf("configure %s per-repo verification: %w", tierLabel, err)
	}

	if _, err := fmt.Fprintf(output, "%s per-repo verify: address=%s role=%s sa=%s/%s repo=%s paths=%d\n",
		tierLabel, releaseConfig.vaultAddress, roleName,
		releaseConfig.pipelineNamespace, saName, repo, len(paths)); err != nil {
		return err
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetched, err := client.FetchKV(deadlineCtx, paths)
	if err != nil {
		_, _ = fmt.Fprintf(output, "common causes for %s per-repo verify failure:\n"+
			"  - the per-repo ServiceAccount %s does not exist\n"+
			"      -> run \"oberth install --install-secretstore --upgrade\"\n"+
			"  - the Vault role %s is not bound to this SA\n"+
			"      -> re-run the installer to provision the role\n"+
			"  - the Vault policy does not cover the requested path\n"+
			"      -> check that the grant exists (oberth access list)\n",
			tierLabel, saName, roleName)
		return fmt.Errorf("%s per-repo verify failed: %w", tierLabel, err)
	}
	defer zeroFetchedSecrets(fetched)
	for _, path := range paths {
		label := path
		if declared, scoped := declaredByFetch[path]; scoped {
			label = declared + " -> " + path
		}
		if _, err := fmt.Fprintf(output, "  ok %s (%d keys)\n", label, len(fetched[path])); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(output, "%s per-repo verify: OK — TokenRequest, %s SA login (%s), Vault auth, read policy, and TLS all verified\n",
		tierLabel, tierLabel, saName)
	return err
}

const secretStoreVerifyHints = `common causes:
  - role or ServiceAccount binding missing on the store side
      -> re-run scripts/setup-secretstore.sh next to the store
  - TokenReview rejected for an out-of-cluster store
      -> the chart's system:auth-delegator binding (secretstore.createAuthDelegatorBinding) must exist
  - the store cannot reach this cluster's API server
      -> check the kubernetes_host that setup wrote (setup-secretstore.sh --kubernetes-host)
  - path missing, deleted, or outside the store-side read policy
      -> check the KV API path (KV v2 paths include /data/) and the policy printed by setup
`

const releaseTierVerifyHints = `common causes:
  - the release ServiceAccount does not exist in the pipeline namespace
      -> ensure the chart's argo-identities template rendered correctly
  - the Oberth ServiceAccount lacks serviceaccounts/token create permission
      -> ensure the chart's rbac-argo template includes serviceaccounts/token create
  - the OpenBao Kubernetes auth role is not bound to the release ServiceAccount
      -> bind: bao write auth/kubernetes/role/<role> bound_service_account_names=<sa> bound_service_account_namespaces=<ns>
  - argo.vault.caCert is empty or missing from the chart values
      -> the release tier cannot verify an internal OpenBao without its CA certificate
  - path missing, deleted, or outside the store-side read policy
      -> the release role must have read access to the same paths the server allowlists
  - TokenReview rejected: the store cannot reach this cluster's API server
      -> check the kubernetes_host on the OpenBao auth mount
`

func zeroFetchedSecrets(fetched map[string]map[string][]byte) {
	for _, values := range fetched {
		for _, value := range values {
			for index := range value {
				value[index] = 0
			}
		}
	}
}

// sortedFieldNames returns the keys of a KV map in sorted order,
// for deterministic verification output. Values are never exposed.
func sortedFieldNames(fields map[string][]byte) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// verifyExpectedKeys compares the actual field names of each fetched KV path
// against the expectations declared in --expect flags. Each expect arg has
// the form "<path-base>/<field>[,<field>,...]" where path-base matches
// filepath.Base of a fetched path.
//
// Missing fields (expected but absent) and unexpected fields (present but not
// expected) are both reported as errors: the expectation set is exhaustive so
// any discrepancy is a drift signal that would fail a credentialed step.
func verifyExpectedKeys(output io.Writer, fetched map[string]map[string][]byte, paths []string, expects []string) error {
	expectedByBase := make(map[string]map[string]struct{})
	for _, expect := range expects {
		slash := strings.IndexByte(expect, '/')
		if slash <= 0 || slash >= len(expect)-1 {
			return fmt.Errorf("%w: invalid --expect %q: must be <path-base>/<field>[,<field>,...]", errUsage, expect)
		}
		base := expect[:slash]
		for _, field := range strings.Split(expect[slash+1:], ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				return fmt.Errorf("%w: invalid --expect %q: empty field name", errUsage, expect)
			}
			if expectedByBase[base] == nil {
				expectedByBase[base] = make(map[string]struct{})
			}
			expectedByBase[base][field] = struct{}{}
		}
	}
	actualByBase := make(map[string]map[string]struct{})
	for _, p := range paths {
		base := filepath.Base(p)
		actual := make(map[string]struct{}, len(fetched[p]))
		for key := range fetched[p] {
			actual[key] = struct{}{}
		}
		if existing, ok := actualByBase[base]; ok {
			for key := range actual {
				existing[key] = struct{}{}
			}
		} else {
			actualByBase[base] = actual
		}
	}
	var problems []string
	for base, expectedFields := range expectedByBase {
		actual, ok := actualByBase[base]
		if !ok {
			problems = append(problems, fmt.Sprintf("  FAIL %s: no verified path has base %q", base, base))
			continue
		}
		var missing, unexpected []string
		for field := range expectedFields {
			if _, ok := actual[field]; !ok {
				missing = append(missing, field)
			}
		}
		for field := range actual {
			if _, ok := expectedFields[field]; !ok {
				unexpected = append(unexpected, field)
			}
		}
		sort.Strings(missing)
		sort.Strings(unexpected)
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("  FAIL %s: missing fields: %s", base, strings.Join(missing, ", ")))
		}
		if len(unexpected) > 0 {
			problems = append(problems, fmt.Sprintf("  FAIL %s: unexpected fields: %s", base, strings.Join(unexpected, ", ")))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		for _, p := range problems {
			_, _ = fmt.Fprintln(output, p)
		}
		return fmt.Errorf("secret store verify -keys: %d field name problem(s)", len(problems))
	}
	return nil
}

// secretStoreConfigFromServeCmdline reads the live serve process's own command
// line (PID 1 in the Oberth pod) and extracts exactly the --secretstore-*
// flags, so verification always tests the configuration the server actually
// runs with — not a hand-retyped copy that could hide a drift.
//
// The scanner is deliberately tolerant of every spelling the Go flag package
// accepts (-flag=value, --flag=value, -flag value, --flag value) and ignores
// all non-secretstore flags; TestSecretStoreCmdlineMatchesServeParsing pins it
// against parseServeOptions so the two can never diverge silently.
func secretStoreConfigFromServeCmdline(path string) (secretStoreVerifyConfig, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc/1/cmdline, overridden only by tests.
	if err != nil {
		return secretStoreVerifyConfig{}, fmt.Errorf("read serve process command line: %w", err)
	}
	if len(raw) > maximumServeCmdlineBytes {
		return secretStoreVerifyConfig{}, errors.New("serve process command line exceeds the size bound")
	}
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) < 2 || !strings.HasSuffix(fields[0], "oberth") || fields[1] != "serve" {
		return secretStoreVerifyConfig{}, errors.New("PID 1 is not an oberth serve process")
	}
	var config secretStoreVerifyConfig
	arguments := fields[2:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if len(argument) < 2 || argument[0] != '-' {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-")
		value := ""
		hasValue := false
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value = name[equals+1:]
			name = name[:equals]
			hasValue = true
		}
		consumeValue := func() error {
			if hasValue {
				return nil
			}
			index++
			if index >= len(arguments) {
				return fmt.Errorf("serve flag --%s has no value", name)
			}
			value = arguments[index]
			return nil
		}
		switch name {
		case "secretstore-address":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.address = value
		case "secretstore-k8s-auth-mount":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.authMount = value
		case "secretstore-role":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.role = value
		case "secretstore-ca-cert":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.caCertPath = value
		case "secretstore-sa-token":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.saTokenPath = value
		case "secretstore-kv-mount":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.kvMount = value
		case "secretstore-path":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.paths = append(config.paths, value)
		case "secretstore-insecure-http":
			// The Go flag package accepts booleans only as -flag or -flag=value.
			enabled := true
			if hasValue {
				parsed, parseErr := strconv.ParseBool(value)
				if parseErr != nil {
					return secretStoreVerifyConfig{}, fmt.Errorf("serve flag --secretstore-insecure-http has invalid value %q", value)
				}
				enabled = parsed
			}
			config.insecureHTTP = enabled
		}
	}
	return config, nil
}

// releaseTierConfigFromServeCmdline reads the live serve process's command line
// and extracts the --argo-* flags that configure the release-tier secret store
// trust chain. The scanner follows the same tolerant pattern as
// secretStoreConfigFromServeCmdline; TestReleaseTierCmdlineMatchesServeParsing
// pins it against parseServeOptions.
func releaseTierConfigFromServeCmdline(path string) (releaseTierVerifyConfig, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc/1/cmdline, overridden only by tests.
	if err != nil {
		return releaseTierVerifyConfig{}, fmt.Errorf("read serve process command line: %w", err)
	}
	if len(raw) > maximumServeCmdlineBytes {
		return releaseTierVerifyConfig{}, errors.New("serve process command line exceeds the size bound")
	}
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) < 2 || !strings.HasSuffix(fields[0], "oberth") || fields[1] != "serve" {
		return releaseTierVerifyConfig{}, errors.New("PID 1 is not an oberth serve process")
	}
	var config releaseTierVerifyConfig
	arguments := fields[2:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if len(argument) < 2 || argument[0] != '-' {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-")
		value := ""
		hasValue := false
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value = name[equals+1:]
			name = name[:equals]
			hasValue = true
		}
		consumeValue := func() error {
			if hasValue {
				return nil
			}
			index++
			if index >= len(arguments) {
				return fmt.Errorf("serve flag --%s has no value", name)
			}
			value = arguments[index]
			return nil
		}
		switch name {
		case "argo-vault-address":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultAddress = value
		case "argo-vault-ca-cert":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultCACertPath = value
		case "argo-vault-credentialed-role":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultCredentialedRole = value
		case "argo-credentialed-serviceaccount":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.credentialedAccount = value
		case "argo-namespace":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.pipelineNamespace = value
		}
	}
	return config, nil
}

// runSecretStoreSync re-derives every Vault policy and role that depends on
// the approval table — per-repo identities (release + CI tiers) and the
// shared credentialed and CI-secrets policies — without touching anything
// else (no mounts, no transit keys, no chart install, no helm).
//
// This is the targeted alternative to `oberth install --install-secretstore
// --upgrade` for the specific case where `access_allow` recorded a new grant
// and the Vault policy needs to include it.
//
// It runs under the administrator's own BAO_TOKEN (from the environment),
// exactly like the full install's secret-store path does. The Oberth server
// has no code path that accepts a store admin token — the sync is an
// operator-session action by design.
func runSecretStoreSync(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("secretstore sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	namespace := flags.String("namespace", installer.DefaultNamespace, "Oberth namespace")
	openbaoNamespace := flags.String("openbao-namespace", installer.DefaultOpenBaoNamespace, "OpenBao namespace")
	argoNamespace := flags.String("argo-namespace", installer.DefaultArgoNamespace, "pipeline namespace")
	kubeContext := flags.String("context", "", "kubeconfig context (default: current context)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: secretstore sync accepts flags only", errUsage)
	}

	rootToken := os.Getenv("BAO_TOKEN")
	if rootToken == "" {
		rootToken = os.Getenv("VAULT_TOKEN")
	}
	if rootToken == "" {
		return errors.New("BAO_TOKEN (or VAULT_TOKEN) must be set: " +
			"secretstore sync runs under the administrator's own store session, " +
			"not the server's identity. " +
			"Set it without exposing it in shell history:\n\n" +
			"    read -rs BAO_TOKEN && export BAO_TOKEN\n" +
			"    oberth secretstore sync")
	}

	kube, _, resolvedContext, err := installer.LoadKubeConfigForContext(*kubeContext, output)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	_ = resolvedContext

	// Read per-repo identities from the running Oberth server.
	_, _ = fmt.Fprintln(output, "Reading per-repo identities from the running server...")
	identities, warnings, produceErr := installer.ProducePerRepoIdentities(
		ctx, kube, installer.DefaultRunCommand, *kubeContext, *namespace,
	)
	for _, warn := range warnings {
		_, _ = fmt.Fprintf(output, "  WARNING: %s\n", warn)
	}
	if produceErr != nil {
		return fmt.Errorf("read per-repo identities: %w", produceErr)
	}
	_, _ = fmt.Fprintf(output, "  %d per-repo identities found\n", len(identities))

	// Find the OpenBao pod. The sync needs the pod to be running; it does not
	// poll like the installer does because the operator expects immediate
	// feedback from a targeted command.
	pod, err := kube.CoreV1().Pods(*openbaoNamespace).Get(ctx, "openbao-0", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("find OpenBao pod %s/openbao-0: %w", *openbaoNamespace, err)
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("OpenBao pod %s/openbao-0 is not running (phase: %s)", *openbaoNamespace, pod.Status.Phase)
	}

	// Run the sync.
	results, syncErr := installer.RunSync(
		ctx, installer.DefaultRunCommand, rootToken,
		*kubeContext, *openbaoNamespace, "openbao-0", *argoNamespace,
		identities,
	)
	for _, r := range results {
		status := "unchanged"
		if r.Changed {
			status = "synced"
		}
		_, _ = fmt.Fprintf(output, "  %s: %s\n", r.Name, status)
	}
	if syncErr != nil {
		return fmt.Errorf("sync grant policies: %w", syncErr)
	}

	_, _ = fmt.Fprintln(output, "secretstore sync: OK — all policies and roles match the approval table")
	return nil
}
