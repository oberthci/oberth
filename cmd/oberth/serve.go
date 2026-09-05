package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type stringList []string

var witnessChainResetUUIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{64}|[0-9a-f]{80})$`)

// splitRunnerImagePrefixes parses the comma-separated allowlist flag; empty
// items are dropped so a trailing comma cannot silently allow everything.
func splitRunnerImagePrefixes(value string) []string {
	var prefixes []string
	for prefix := range strings.SplitSeq(value, ",") {
		prefix = strings.TrimSpace(prefix)
		if prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type serveOptions struct {
	dataRoot                string
	database                string
	sshListen               string
	httpsListen             string
	tlsCert                 string
	tlsKey                  string
	sshHostKey              string
	upstreamKey             string
	knownHosts              string
	namespace               string
	runnerImagePrefixes     string
	scheduleMinInterval     time.Duration
	scheduleMaxEntries      int
	fragmentAllowlist       string
	artifactsLimitBytes     int64
	artifactsBudgetBytes    int64
	maxConcurrent           int
	ciCacheRoot             string
	releaseCacheRoot        string
	secretStoreAddress      string
	secretStoreAuthMount    string
	secretStoreRole         string
	secretStoreCACert       string
	secretStoreSAToken      string
	secretStorePaths        stringList
	secretStoreKVMount      string
	secretStoreTransitMount string
	secretStoreTransitKey   string
	secretStoreInsecureHTTP bool

	// Argo execution engine. Leaving argoNamespace empty leaves the engine
	// off entirely, which is the default: every repository then runs on the
	// Kubernetes Job engine exactly as before.
	argoNamespace             string
	argoPipelineAccount       string
	argoCredentialedAccount   string
	argoCISecretsAccount      string
	argoExecutorAccount       string
	argoVaultAddress          string
	argoVaultCredentialedRole string
	argoVaultCISecretsRole    string
	argoVaultCACert           string
	argoWorkflowTimeout       time.Duration
	argoWorkflowTTL           int
	// argoSourceStorageClass names the class for the per-run source claim the
	// server creates in the pipeline namespace. Empty selects the cluster
	// default, which is what a single-node install wants.
	argoSourceStorageClass string

	gitCommandTimeout       time.Duration
	gcInterval              time.Duration
	pushBannerURL           string
	auditTSAURL             string
	auditRekorURL           string
	auditRekorInsecureHTTP  bool
	auditTSAInsecureHTTP    bool
	auditTSARoots           string
	auditRekorCA            string
	auditRekorPubKey        string
	auditTSACA              string
	auditAnchorInterval     time.Duration
	auditAnchorMaxAge       time.Duration
	acceptWitnessChainReset string
	acceptWitnessGenesis    string
}

func runServe(ctx context.Context, arguments []string, output io.Writer) error {
	options, err := parseServeOptions(arguments, output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	logger := log.New(output, "oberth: ", log.LstdFlags|log.LUTC)
	return serve(ctx, options, logger)
}

func parseServeOptions(arguments []string, output io.Writer) (serveOptions, error) {
	var options serveOptions
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.dataRoot, "data", "/data", "persistent data root")
	flags.StringVar(&options.database, "database", "/data/oberth.sqlite", "SQLite database path")
	flags.StringVar(&options.sshListen, "ssh-listen", ":2222", "SSH listen address")
	flags.StringVar(&options.httpsListen, "https-listen", ":8443", "HTTPS listen address")
	flags.StringVar(&options.tlsCert, "tls-cert", "/etc/oberth/tls/tls.crt", "TLS certificate")
	flags.StringVar(&options.tlsKey, "tls-key", "/etc/oberth/tls/tls.key", "TLS private key")
	flags.StringVar(&options.sshHostKey, "ssh-host-key", "/etc/oberth/ssh/ssh_host_key", "SSH host private key")
	flags.StringVar(&options.upstreamKey, "upstream-key", "/etc/oberth/upstream-key/id_ed25519", "upstream SSH private key")
	flags.StringVar(&options.knownHosts, "known-hosts", "/etc/oberth/known-hosts/known_hosts", "upstream known_hosts")
	flags.StringVar(&options.namespace, "namespace", "oberth", "Kubernetes namespace")
	flags.StringVar(&options.runnerImagePrefixes, "runner-image-prefixes", strings.Join(periapsis.DefaultRunnerImagePrefixes, ","), "comma-separated allowlist of permitted runner image prefixes")
	flags.DurationVar(&options.scheduleMinInterval, "schedule-min-interval", defaultScheduleMinInterval, "shortest interval a repository may schedule itself at")
	flags.IntVar(&options.scheduleMaxEntries, "schedule-max-entries", defaultScheduleMaxEntries, "most schedule entries one repository may declare")
	flags.StringVar(&options.fragmentAllowlist, "fragment-allowlist", "", "comma-separated repositories usable as pipeline fragments; empty permits every registered repository")
	flags.Int64Var(&options.artifactsLimitBytes, "artifacts-limit-bytes", defaultArtifactsLimitBytes, "maximum total bytes of artifacts kept per run")
	flags.Int64Var(&options.artifactsBudgetBytes, "artifacts-budget-bytes", defaultArtifactsBudgetBytes, "total artifact storage before the oldest runs are evicted")
	flags.IntVar(&options.maxConcurrent, "max-concurrent-jobs", 3, "maximum concurrent Jobs")
	flags.StringVar(&options.ciCacheRoot, "ci-cache-root", "/var/cache/oberth/ci", "CI host cache root")
	flags.StringVar(&options.releaseCacheRoot, "release-cache-root", "/var/cache/oberth/release", "release host cache root")
	flags.StringVar(&options.secretStoreAddress, "secretstore-address", "", "OpenBao API base URL for store-sourced release secrets (HTTPS)")
	flags.StringVar(&options.secretStoreAuthMount, "secretstore-k8s-auth-mount", secretstore.DefaultAuthMountPath, "OpenBao Kubernetes auth mount path")
	flags.StringVar(&options.secretStoreRole, "secretstore-role", "", "OpenBao Kubernetes auth role bound to the Oberth ServiceAccount")
	flags.StringVar(&options.secretStoreCACert, "secretstore-ca-cert", "", "optional PEM file pinning the OpenBao TLS trust anchors")
	flags.StringVar(&options.secretStoreSAToken, "secretstore-sa-token", "", "optional ServiceAccount token path override for the OpenBao login")
	flags.Var(&options.secretStorePaths, "secretstore-path", "allowlisted OpenBao KV path (repeatable)")
	flags.StringVar(&options.secretStoreKVMount, "secretstore-kv-mount", secretstore.DefaultKVMount, "KV v2 mount backing the virtual oberth/upstream/ secret namespace")
	flags.StringVar(&options.secretStoreTransitMount, "secretstore-transit-mount", "", "OpenBao transit mount for trusted Plan artifacts (enables trusted Plan/Apply with --secretstore-transit-key)")
	flags.StringVar(&options.secretStoreTransitKey, "secretstore-transit-key", "", "OpenBao transit key for trusted Plan artifacts")
	flags.BoolVar(&options.secretStoreInsecureHTTP, "secretstore-insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address")
	flags.StringVar(&options.argoNamespace, "argo-namespace", "", "namespace for Argo Workflow pipelines (required)")
	flags.StringVar(&options.argoSourceStorageClass, "argo-source-storage-class", "", "storage class for per-run pipeline source volumes (empty: cluster default)")
	flags.StringVar(&options.argoPipelineAccount, "argo-pipeline-serviceaccount", "oberth-argo-pipeline", "ServiceAccount for pipeline templates without approved secrets; no Vault role, no token")
	flags.StringVar(&options.argoCredentialedAccount, "argo-credentialed-serviceaccount", "oberth-argo-credentialed", "ServiceAccount for release templates with approved secrets; carries a projected token for OpenBao Kubernetes auth")
	flags.StringVar(&options.argoCISecretsAccount, "argo-ci-secrets-serviceaccount", "oberth-argo-ci-secrets", "ServiceAccount for CI templates with approved upstream-scoped secrets; its Vault role never carries release grants")
	flags.StringVar(&options.argoExecutorAccount, "argo-executor-serviceaccount", "oberth-argo-executor", "ServiceAccount for Argo's own executor containers; must differ from all pipeline identities")
	flags.StringVar(&options.argoVaultAddress, "argo-vault-address", "", "OpenBao API base URL injected into credentialed Argo containers as VAULT_ADDR (HTTPS)")
	flags.StringVar(&options.argoVaultCredentialedRole, "argo-vault-credentialed-role", "", "OpenBao Kubernetes auth role for credentialed release pipelines; bound to the credentialed ServiceAccount and namespace")
	flags.StringVar(&options.argoVaultCISecretsRole, "argo-vault-ci-secrets-role", "", "OpenBao Kubernetes auth role for CI pipelines with approved upstream-scoped secrets; bound to the CI-secrets ServiceAccount and namespace (empty refuses CI credentialed pipelines at admission)")
	flags.StringVar(&options.argoVaultCACert, "argo-vault-ca-cert", "", "PEM file pinning the trust anchors credentialed pipeline containers verify --argo-vault-address against")
	flags.DurationVar(&options.argoWorkflowTimeout, "argo-workflow-timeout", 12*time.Hour, "ceiling on a Workflow's own activeDeadlineSeconds")
	flags.IntVar(&options.argoWorkflowTTL, "argo-workflow-ttl", 3600, "finished Workflow retention in seconds")
	flags.DurationVar(&options.gitCommandTimeout, "git-timeout", 10*time.Minute, "Git command timeout")
	flags.DurationVar(&options.gcInterval, "git-gc-interval", 24*time.Hour, "periodic Git GC interval")
	flags.StringVar(&options.pushBannerURL, "push-banner-url", "", "optional HTTPS dashboard URL echoed to every accepted git push")
	flags.StringVar(&options.auditTSAURL, "audit-tsa-url", "", "RFC 3161 timestamp authority HTTPS URL (empty disables external timestamps)")
	flags.StringVar(&options.auditRekorURL, "audit-rekor-url", "", "Rekor transparency log HTTPS URL (empty disables the external witness)")
	flags.BoolVar(&options.auditRekorInsecureHTTP, "audit-rekor-insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP Rekor URL")
	flags.BoolVar(&options.auditTSAInsecureHTTP, "audit-tsa-insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP TSA URL")
	flags.StringVar(&options.auditTSARoots, "audit-tsa-roots", "", "optional PEM file containing pinned TSA trust roots")
	flags.StringVar(&options.auditRekorCA, "audit-rekor-ca", "", "optional PEM file pinning the Rekor HTTPS TLS trust anchors")
	flags.StringVar(&options.auditRekorPubKey, "audit-rekor-pubkey", "", "optional PEM public key pinning the Rekor log identity instead of Sigstore TUF")
	flags.StringVar(&options.auditTSACA, "audit-tsa-ca", "", "optional PEM file pinning the TSA HTTPS TLS trust anchors")
	flags.DurationVar(&options.auditAnchorInterval, "audit-anchor-interval", 10*time.Minute, "external audit checkpoint interval")
	flags.DurationVar(&options.auditAnchorMaxAge, "audit-anchor-max-age", 30*time.Minute, "maximum checkpoint age allowed by readiness")
	flags.StringVar(&options.acceptWitnessChainReset, "accept-witness-chain-reset", "",
		"one-shot acknowledgment: exact Rekor UUID of the latest published witness before a fresh genesis permanently abandons that chain")
	flags.StringVar(&options.acceptWitnessGenesis, "accept-witness-genesis", "",
		"one-shot acknowledgment: exact current audit chain head as <auditID>:<sha256hex>; "+
			"marks it as the witness genesis so external witnessing starts there, explicitly "+
			"trusting all prior local history (enables Rekor on an existing deployment)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return serveOptions{}, flag.ErrHelp
		}
		return serveOptions{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return serveOptions{}, fmt.Errorf("%w: serve accepts flags only", errUsage)
	}
	if err := validateServeOptions(options); err != nil {
		return serveOptions{}, err
	}
	return options, nil
}

func validateServeOptions(options serveOptions) error {
	root := filepath.Clean(options.dataRoot)
	database := filepath.Clean(options.database)
	if !filepath.IsAbs(root) || options.dataRoot != root || !pathWithin(root, database) {
		return errors.New("serve: database must be a clean absolute path inside the data root")
	}
	if options.sshListen == options.httpsListen || strings.TrimSpace(options.sshListen) == "" || strings.TrimSpace(options.httpsListen) == "" {
		return errors.New("serve: distinct SSH and HTTPS listen addresses are required")
	}
	if options.argoNamespace == "" {
		return errors.New("serve: --argo-namespace is required")
	}
	if err := validateArgoServeOptions(options); err != nil {
		return err
	}
	if options.maxConcurrent <= 0 || options.maxConcurrent > 64 || options.gitCommandTimeout <= 0 || options.gcInterval <= 0 {
		return errors.New("serve: concurrency, timeout, and GC values must be positive")
	}
	if strings.TrimSpace(options.namespace) == "" {
		return errors.New("serve: namespace is required")
	}
	if err := periapsis.ValidateRunnerImagePrefixes(splitRunnerImagePrefixes(options.runnerImagePrefixes)); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if options.auditTSAURL != "" && !validAuditHTTPSURL(options.auditTSAURL) {
		return errors.New("serve: audit TSA URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if options.auditRekorURL != "" && !validAuditURL(options.auditRekorURL, options.auditRekorInsecureHTTP) {
		if options.auditRekorInsecureHTTP {
			return errors.New("serve: audit Rekor URL must be an absolute HTTP or HTTPS URL without credentials or a fragment")
		}
		return errors.New("serve: audit Rekor URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if options.auditAnchorInterval <= 0 || options.auditAnchorMaxAge < options.auditAnchorInterval {
		return errors.New("serve: audit anchor interval must be positive and max age must be at least the interval")
	}
	if options.auditTSAURL == "" && (options.auditTSARoots != "" || options.auditTSACA != "") {
		return errors.New("serve: audit TSA trust flags require --audit-tsa-url")
	}
	if options.auditRekorURL == "" && options.auditRekorCA != "" {
		return errors.New("serve: --audit-rekor-ca requires --audit-rekor-url")
	}
	if options.auditRekorURL == "" && options.auditRekorPubKey != "" {
		return errors.New("serve: --audit-rekor-pubkey requires --audit-rekor-url")
	}
	if options.auditRekorURL == "" && options.auditRekorInsecureHTTP {
		return errors.New("serve: --audit-rekor-insecure-http requires --audit-rekor-url")
	}
	if options.auditRekorURL == "" && options.acceptWitnessChainReset != "" {
		return errors.New("serve: --accept-witness-chain-reset requires the external Rekor witness (--audit-rekor-url)")
	}
	if options.auditRekorURL == "" && options.acceptWitnessGenesis != "" {
		return errors.New("serve: --accept-witness-genesis requires the external Rekor witness (--audit-rekor-url)")
	}
	if options.acceptWitnessChainReset != "" && options.acceptWitnessGenesis != "" {
		return errors.New("serve: --accept-witness-genesis and --accept-witness-chain-reset are mutually exclusive")
	}
	if options.acceptWitnessGenesis != "" && !witnessGenesisAcknowledgmentPattern.MatchString(options.acceptWitnessGenesis) {
		return errors.New("serve: accept-witness-genesis must be the exact audit chain head as <auditID>:<sha256hex> (positive integer, colon, 64 lowercase hex characters)")
	}
	if options.acceptWitnessGenesis != "" {
		parts := splitOnce(options.acceptWitnessGenesis, ':')
		if len(parts) == 2 {
			if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
				return errors.New("serve: accept-witness-genesis audit ID exceeds int64 range")
			}
		}
	}
	if options.auditTSARoots != "" && (!filepath.IsAbs(options.auditTSARoots) || filepath.Clean(options.auditTSARoots) != options.auditTSARoots) {
		return errors.New("serve: audit TSA roots must be a clean absolute path")
	}
	if options.auditRekorCA != "" && (!filepath.IsAbs(options.auditRekorCA) || filepath.Clean(options.auditRekorCA) != options.auditRekorCA) {
		return errors.New("serve: audit Rekor CA certificate must be a clean absolute path")
	}
	if options.auditRekorPubKey != "" && (!filepath.IsAbs(options.auditRekorPubKey) || filepath.Clean(options.auditRekorPubKey) != options.auditRekorPubKey) {
		return errors.New("serve: audit Rekor public key must be a clean absolute path")
	}
	if options.auditTSACA != "" && (!filepath.IsAbs(options.auditTSACA) || filepath.Clean(options.auditTSACA) != options.auditTSACA) {
		return errors.New("serve: audit TSA CA certificate must be a clean absolute path")
	}
	if options.acceptWitnessChainReset != "" && !witnessChainResetUUIDPattern.MatchString(options.acceptWitnessChainReset) {
		return errors.New("serve: accept-witness-chain-reset must be the exact Rekor UUID (64 or 80 lowercase hex characters) of the latest published witness")
	}
	if options.pushBannerURL != "" && !validAuditHTTPSURL(options.pushBannerURL) {
		return errors.New("serve: push banner URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	return validateSecretStoreServeOptions(options)
}

func validateSecretStoreServeOptions(options serveOptions) error {
	if options.secretStoreAddress == "" {
		if options.secretStoreRole != "" || options.secretStoreCACert != "" || options.secretStoreSAToken != "" ||
			len(options.secretStorePaths) != 0 || options.secretStoreInsecureHTTP || options.secretStoreAuthMount != secretstore.DefaultAuthMountPath ||
			options.secretStoreKVMount != secretstore.DefaultKVMount || options.secretStoreTransitMount != "" || options.secretStoreTransitKey != "" {
			return errors.New("serve: secret store flags require --secretstore-address")
		}
		return nil
	}
	if strings.TrimSpace(options.secretStoreRole) == "" {
		return errors.New("serve: --secretstore-address requires --secretstore-role")
	}
	if options.secretStoreCACert != "" && (!filepath.IsAbs(options.secretStoreCACert) || filepath.Clean(options.secretStoreCACert) != options.secretStoreCACert) {
		return errors.New("serve: secret store CA certificate must be a clean absolute path")
	}
	if options.secretStoreSAToken != "" && (!filepath.IsAbs(options.secretStoreSAToken) || filepath.Clean(options.secretStoreSAToken) != options.secretStoreSAToken) {
		return errors.New("serve: secret store ServiceAccount token must be a clean absolute path")
	}
	transitEnabled := options.secretStoreTransitMount != "" || options.secretStoreTransitKey != ""
	if transitEnabled && (options.secretStoreTransitMount == "" || options.secretStoreTransitKey == "") {
		return errors.New("serve: trusted Plan transit requires both --secretstore-transit-mount and --secretstore-transit-key")
	}
	if transitEnabled {
		if options.secretStoreInsecureHTTP || !strings.HasPrefix(options.secretStoreAddress, "https://") {
			return errors.New("serve: trusted Plan transit requires a verified HTTPS secret store and forbids the insecure HTTP development override")
		}
		if err := validateSecretStoreSingleSegment("transit mount", options.secretStoreTransitMount); err != nil {
			return err
		}
		if err := validateSecretStoreSingleSegment("transit key", options.secretStoreTransitKey); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretStoreSingleSegment(label, value string) error {
	if strings.Contains(value, "/") {
		return fmt.Errorf("serve: secret store %s must be one clean non-reserved path segment", label)
	}
	if err := periapsis.ValidateSecretStorePath(value); err != nil {
		return fmt.Errorf("serve: secret store %s: %w", label, err)
	}
	return nil
}

func validAuditHTTPSURL(raw string) bool {
	return validAuditURL(raw, false)
}

func validAuditURL(raw string, insecureHTTP bool) bool {
	if strings.Contains(raw, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return strings.HasPrefix(raw, "https://")
	case "http":
		return insecureHTTP && strings.HasPrefix(raw, "http://")
	default:
		return false
	}
}

func serve(ctx context.Context, options serveOptions, logger *log.Logger) (result error) {
	if err := os.MkdirAll(options.dataRoot, 0o700); err != nil {
		return fmt.Errorf("create data root: %w", err)
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster Kubernetes config: %w", err)
	}
	restConfig.Timeout = 10 * time.Second
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	continuity, err := auditanchor.NewKubernetesContinuity(kube, options.namespace)
	if err != nil {
		return err
	}
	hostKey, err := readBoundedFile(options.sshHostKey, 1<<20)
	if err != nil {
		return fmt.Errorf("read SSH host key: %w", err)
	}
	var tsaClient *auditanchor.Client
	if options.auditTSAURL != "" {
		tsaRoots, rootsErr := loadTSARoots(options.auditTSARoots)
		if rootsErr != nil {
			return rootsErr
		}
		tsaCACert, caErr := loadOptionalCACert(options.auditTSACA)
		if caErr != nil {
			return fmt.Errorf("load TSA CA certificate: %w", caErr)
		}
		tsaClient, err = auditanchor.NewClient(auditanchor.ClientConfig{Endpoint: options.auditTSAURL, Roots: tsaRoots, TLSCACert: tsaCACert, InsecureHTTP: options.auditTSAInsecureHTTP})
		if err != nil {
			return err
		}
	}
	var rekorWitness *auditanchor.RekorWitness
	if options.auditRekorURL != "" {
		rekorCACert, caErr := loadOptionalCACert(options.auditRekorCA)
		if caErr != nil {
			return fmt.Errorf("load Rekor CA certificate: %w", caErr)
		}
		rekorPublicKey, pubKeyErr := loadRekorPublicKey(options.auditRekorPubKey)
		if pubKeyErr != nil {
			return fmt.Errorf("load Rekor public key: %w", pubKeyErr)
		}
		rekorWitness, err = auditanchor.NewRekorWitness(ctx, hostKey, auditanchor.RekorWitnessConfig{
			Endpoint: options.auditRekorURL, TLSCACert: rekorCACert, PublicKey: rekorPublicKey,
			InsecureHTTP: options.auditRekorInsecureHTTP,
		})
		if err != nil {
			return err
		}
	}
	switch {
	case tsaClient != nil && rekorWitness != nil:
		logger.Printf("audit anchoring: external (TSA %s, Rekor witness %s)", options.auditTSAURL, options.auditRekorURL)
	case tsaClient != nil:
		logger.Printf("audit anchoring: signed timestamps only (TSA %s); no external witness configured", options.auditTSAURL)
	case rekorWitness != nil:
		logger.Printf("audit anchoring: external witness only (Rekor %s); no timestamp authority configured", options.auditRekorURL)
	default:
		logger.Printf("audit anchoring: local hash chain only; no external witness or timestamp authority configured")
	}
	// newAuditManagerConfig assigns only non-nil implementations: a typed-nil
	// interface value would read as "configured" inside the manager and defeat
	// the local-only mode.
	newAuditManagerConfig := func(anchorStore auditanchor.AnchorStore) auditanchor.ManagerConfig {
		config := auditanchor.ManagerConfig{
			Store: anchorStore, Continuity: continuity,
			Interval: options.auditAnchorInterval, MaxAge: options.auditAnchorMaxAge, Logger: logger,
		}
		if tsaClient != nil {
			config.Authority = tsaClient
		}
		if rekorWitness != nil {
			config.Witness = rekorWitness
		}
		return config
	}
	reset := witnessChainReset{logger: logger}
	if options.acceptWitnessChainReset != "" {
		acknowledged, normalizeErr := auditanchor.NormalizeRekorUUID(options.acceptWitnessChainReset)
		if normalizeErr != nil {
			return fmt.Errorf("accept-witness-chain-reset: %w", normalizeErr)
		}
		reset.acknowledgedTip = acknowledged
		reset.chain = rekorWitness
	}
	adoption := witnessGenesisAdoption{logger: logger}
	if options.acceptWitnessGenesis != "" {
		baselineID, baselineSHA256, parseErr := parseWitnessGenesisFlag(options.acceptWitnessGenesis)
		if parseErr != nil {
			return fmt.Errorf("accept-witness-genesis: %w", parseErr)
		}
		adoption.baselineID = baselineID
		adoption.baselineSHA256 = baselineSHA256
		adoption.chain = rekorWitness
	}
	witnessRecoveryDeferred := false
	database, err := openStartupDatabase(ctx, options.database, continuity, reset, adoption, func(inspection *store.Store) error {
		verifier, managerErr := auditanchor.NewManager(newAuditManagerConfig(inspection))
		if managerErr != nil {
			return managerErr
		}
		return verifier.VerifyStartup(ctx)
	})
	if err != nil {
		// A witness chain reset requires Rekor to verify the acknowledgment;
		// deferring recovery in that case would violate the operator's explicit
		// intent, so only the normal startup path enters degraded mode.
		if !errors.Is(err, auditanchor.ErrWitnessUnavailable) || options.acceptWitnessChainReset != "" || options.acceptWitnessGenesis != "" {
			return err
		}
		logger.Printf("WARNING: audit witness recovery deferred: Rekor unavailable at startup: %v", err)
		witnessRecoveryDeferred = true
		database, err = store.OpenCurrent(ctx, options.database, store.Options{})
		if err != nil {
			return fmt.Errorf("open database after deferred witness recovery: %w", err)
		}
	}
	closed := false
	closeDatabase := func() error {
		if closed {
			return nil
		}
		closed = true
		return database.Close()
	}
	defer func() { result = errors.Join(result, closeDatabase()) }()
	anchors, err := auditanchor.NewManager(newAuditManagerConfig(database))
	if err != nil {
		return err
	}
	if !witnessRecoveryDeferred {
		if err := anchors.Initialize(ctx); err != nil {
			if !errors.Is(err, auditanchor.ErrWitnessUnavailable) {
				return fmt.Errorf("initialize verified audit continuity: %w", err)
			}
			logger.Printf("WARNING: audit anchor initialization deferred: Rekor unavailable: %v", err)
			witnessRecoveryDeferred = true
		}
	}
	if witnessRecoveryDeferred {
		logger.Printf("WARNING: pod alive (liveness passes) but not ready (readiness blocked, mutations gated)")
		logger.Printf("WARNING: background audit anchor cycle will restore readiness when Rekor becomes available")
	}

	sshCommand, err := buildPerUpstreamSSHCommand(ctx, database, options, "/tmp/oberth-ssh-config", logger)
	if err != nil {
		return err
	}
	upstreams := app.Upstreams{Catalog: database}
	git, err := gitcache.New(gitcache.Config{
		Root: filepath.Join(options.dataRoot, "git"), Upstream: upstreams.Remote,
		CommandTimeout:  options.gitCommandTimeout,
		Env:             map[string]string{"GIT_SSH_COMMAND": sshCommand, "GIT_SSH_VARIANT": "ssh"},
		Logger:          logger,
		PreFinalizeGate: anchors.AllowMutation,
	})
	if err != nil {
		return err
	}
	logs, err := runlog.Open(filepath.Join(options.dataRoot, "logs"))
	if err != nil {
		return err
	}
	fragmentLoader, err := app.NewGitFragmentLoader(git, database, splitRunnerImagePrefixes(options.fragmentAllowlist))
	if err != nil {
		return err
	}
	artifactStore, err := artifacts.Open(filepath.Join(options.dataRoot, "artifacts"))
	if err != nil {
		return err
	}
	perRepoIdentities, err := buildPerRepoIdentities(ctx, database)
	if err != nil {
		return fmt.Errorf("build per-repo identities: %w", err)
	}
	if len(perRepoIdentities) > 0 {
		logger.Printf("per-repo identities: %d repositories with secret grants", len(perRepoIdentities))
	}
	argoJobs, err := buildArgoEngine(options, restConfig, kube, database, database, fragmentLoader,
		artifactStoreAdapter{store: artifactStore, scanPatterns: artifacts.DefaultScanPatterns},
		options.artifactsLimitBytes, options.artifactsBudgetBytes, perRepoIdentities)
	if err != nil {
		return err
	}
	logger.Printf("argo execution engine: namespace=%s pipeline-sa=%s credentialed-sa=%s ci-secrets-sa=%s executor-sa=%s",
		options.argoNamespace, options.argoPipelineAccount, options.argoCredentialedAccount,
		options.argoCISecretsAccount, options.argoExecutorAccount)
	if len(options.secretStorePaths) > 0 {
		logger.Printf("--secretstore-path values are used only by 'oberth secretstore verify' as default paths; admission uses the approval table exclusively")
	}
	signals := service.NewSignals()
	scheduler, err := service.NewScheduler(service.SchedulerConfig{
		Store: database, Git: git, Logs: logs, Jobs: argoJobs, ReleaseJobs: argoJobs, Auditor: database,
		Signals: signals, WorkspaceRoot: filepath.Join(options.dataRoot, "work"), MaxConcurrent: options.maxConcurrent,
		MutationGate: anchors.AllowMutation,
	})
	if err != nil {
		return err
	}
	health := app.Health{Store: database, Audit: anchors.Ready, VCSCache: &app.VCSSnapshot{}, Configured: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The Git transport is configured when at least one complete SSH
		// identity validates: the shared fallback key, or any per-upstream
		// key projected from the Secret volume. A fresh install whose first
		// `upstream add` provisioned only a dedicated key has no shared key
		// and must still become ready; a broken individual upstream surfaces
		// through the per-upstream VCS probe instead.
		_, globalErr := app.GitSSHCommand(options.upstreamKey, options.knownHosts)
		if globalErr == nil {
			return nil
		}
		registered, listErr := database.ListUpstreams(ctx)
		if listErr != nil {
			return globalErr
		}
		keyDirectory := filepath.Dir(options.upstreamKey)
		for _, upstream := range registered {
			if upstream.Kind != "ssh" || upstream.KeyName == "" || !app.ValidUpstreamKeyName(upstream.KeyName) {
				continue
			}
			if _, err := app.GitSSHCommand(filepath.Join(keyDirectory, upstream.KeyName), options.knownHosts); err == nil {
				return nil
			}
		}
		// File-based checks exhausted. Fall back to the Kubernetes API:
		// after `upstream add` stores an SSH key in the Secret, the kubelet
		// may take up to ~60s to project the update into the volume mount.
		// Reading the Secrets directly confirms configuration without
		// waiting for file projection.
		if checkUpstreamSecretConfigured(ctx, kube, options.namespace, options.upstreamKey, options.knownHosts, registered) == nil {
			return nil
		}
		return globalErr
	}, VCS: func(ctx context.Context, upstream model.Upstream) error {
		if upstream.Kind == "local" {
			info, err := os.Stat(upstream.BaseURL)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return errors.New("local upstream is not a directory")
			}
			return nil
		}
		// Per-upstream key: the Secret volume mount projects each dedicated
		// key as a file beside the shared one. Fall back to the shared key
		// while the kubelet has not yet projected a freshly added data key.
		keyPath := options.upstreamKey
		if upstream.KeyName != "" && app.ValidUpstreamKeyName(upstream.KeyName) {
			perUpstreamKey := filepath.Join(filepath.Dir(options.upstreamKey), upstream.KeyName)
			if _, statErr := os.Stat(perUpstreamKey); statErr == nil {
				keyPath = perUpstreamKey
			}
		}
		privateKey, err := readBoundedFile(keyPath, 1<<20)
		if err != nil {
			return fmt.Errorf("read upstream identity: %w", err)
		}
		knownHostsData, err := readBoundedFile(options.knownHosts, 1<<20)
		if err != nil {
			return fmt.Errorf("read upstream host pins: %w", err)
		}
		// Asserts endpoint reachability + pinned host key + accepted identity.
		return app.ProbeSSHAuthentication(ctx, upstream.BaseURL, privateKey, knownHostsData)
	}, Cluster: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := kube.Discovery().ServerVersion()
		return err
	}}
	health.Version = version
	health.Identity = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return app.SSHPublicKeyFingerprint(options.upstreamKey)
	}
	health.SecretStore = secretStoreStatus(options)
	if options.secretStoreAddress != "" {
		probeClient, probeErr := buildSecretStoreProbeClient(options)
		if probeErr != nil {
			return fmt.Errorf("secret store probe: %w", probeErr)
		}
		health.SecretStoreProbe = func(ctx context.Context) error {
			return probeClient.VerifyLogin(ctx)
		}
		health.SecretStoreSealed = func(ctx context.Context) (bool, error) {
			return probeClient.SealStatus(ctx)
		}
		health.SecretStoreCache = &app.SecretStoreProbeSnapshot{}
		// Periodic secret-store prober with transition-based logging (#259).
		// Ticks every 60s; logs only on state transitions (ready/sealed/failing),
		// never on steady state. Exits cleanly on server context cancellation.
		const secretStoreProbeInterval = 60 * time.Second
		observer := &app.SecretStoreObserver{
			Log:     logger.Printf,
			Address: options.secretStoreAddress,
		}
		go func() {
			ticker := time.NewTicker(secretStoreProbeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					result := health.RefreshSecretStore(ctx)
					observer.Observe(result)
				}
			}
		}()
	}
	externallyAnchored := tsaClient != nil || rekorWitness != nil
	health.AuditMode = "local"
	if externallyAnchored {
		health.AuditMode = "anchored"
	}
	health.AuditChain = func(ctx context.Context) (app.AuditChainStatus, error) {
		chain := app.AuditChainStatus{Anchored: externallyAnchored}
		head, err := database.AuditHeadHint(ctx)
		if err != nil {
			return chain, err
		}
		chain.HeadID = head.ID
		chain.HeadSHA256 = hex.EncodeToString(head.SHA256)
		if tsaClient == nil {
			// Without a timestamp authority no signed checkpoint is ever
			// recorded; the local hash chain head is the complete status.
			return chain, nil
		}
		anchor, err := database.LatestAuditAnchor(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return chain, nil
			}
			return chain, err
		}
		chain.AnchorID = anchor.ID
		chain.TSAURL = anchor.TSAURL
		chain.AnchoredAt = anchor.AnchoredAt.UTC().Format(time.RFC3339)
		return chain, nil
	}
	accessReconciler := service.NewAccessReconciler(kube, options.namespace, database, logger)
	if err := accessReconciler.Reconcile(ctx); err != nil {
		logger.Printf("WARNING: initial secret access reconcile failed: %v; credentialed admission is blocked until reconciliation succeeds", err)
	}
	// Wire the reconciler health gate so credentialed admission blocks until
	// the initial ConfigMap read succeeds. Without this, a transient startup
	// failure would leave stale grants active in sqlite for admission to consume.
	argoJobs.SetReconcilerHealth(accessReconciler)
	controlAPI, err := service.NewAPI(service.APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, PromotionRuns: database, Enqueues: scheduler, Git: git,
		Refs: git, Logs: logs, Artifacts: artifactStore, Auditor: database, Health: health, Signals: signals,
		MutationGate:           anchors.AllowMutation,
		PromotionWorkspaceRoot: filepath.Join(options.dataRoot, "work"),
		SecretAccess:           database,
		SecretAccessReconciler: accessReconciler,
		RepositoryRemover:      database,
		RemoveGitCache:         git.RemoveRepository,
	})
	if err != nil {
		return err
	}
	authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: database})
	if err != nil {
		return err
	}
	httpAPI, err := api.New(authenticator, controlAPI, controlAPI, version, api.WithErrorClassifier(classifyViewError))
	if err != nil {
		return err
	}
	pushes, err := service.NewPushIngestor(service.PushConfig{
		Repositories: database, Discoverer: upstreams, Git: git, Receives: database,
		Branches: scheduler, Releases: scheduler,
	})
	if err != nil {
		return err
	}
	receiveHandler, err := service.NewReceiveHandler(pushes)
	if err != nil {
		return err
	}
	hostSigner, err := sshserver.ParseHostSigner(hostKey)
	if err != nil {
		return err
	}
	sshService, err := sshserver.New(sshserver.Config{
		HostSigner: hostSigner, Resolver: app.SSHIdentities{Uplinks: database}, Git: git,
		OnUpdate: receiveHandler, SessionHandler: receiveHandler.WithNotices,
		PushBannerURL: options.pushBannerURL,
		Logger:        logger, MutationGate: anchors.AllowMutation,
	})
	if err != nil {
		return err
	}
	tlsCertificate, err := tls.LoadX509KeyPair(options.tlsCert, options.tlsKey)
	if err != nil {
		return fmt.Errorf("load HTTPS identity: %w", err)
	}
	httpsServer := &http.Server{
		Handler: httpAPI.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		// ReadTimeout bounds the time a client may take to send the complete
		// request (headers + body). 30 seconds is generous for the 1 MiB MCP
		// body limit. Long-poll responses (wait, promote_status, livelog) are
		// unaffected: the body is read in full before the handler's long-poll
		// begins, and the response phase is bounded by WriteTimeout instead.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 3 * time.Minute, MaxHeaderBytes: 1 << 20,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{tlsCertificate}},
	}
	activateScheduler := func(activationCtx context.Context) error {
		if err := waitForAuditIntegrity(activationCtx, anchors.AllowMutation); err != nil {
			return err
		}
		if err := database.RecoverOwnerState(activationCtx); err != nil {
			return fmt.Errorf("recover owner state after audit verification: %w", err)
		}
		if err := sshService.ReplayPending(activationCtx); err != nil {
			return fmt.Errorf("replay accepted Git events after audit verification: %w", err)
		}
		return nil
	}
	schedules := app.NewSchedules(app.SchedulesConfig{
		Repositories: database.ListRepositories,
		Git:          git,
		Runs:         database,
		Enqueuer:     scheduler,
		State:        database,
		Upstreams:    database,
		MinInterval:  options.scheduleMinInterval,
		MaxEntries:   options.scheduleMaxEntries,
	})
	return runComponents(ctx, options, scheduler, anchors, git, sshService, httpsServer, newAdminGateServer(anchors.AllowMutation, options.database), activateScheduler, accessReconciler, schedules, logger)
}

type startupContinuity interface {
	Pinned(context.Context) ([]model.AuditWitness, error)
	Intents(context.Context) ([]model.AuditWitnessIntent, error)
	Prepare(context.Context, model.AuditWitnessIntent) error
}

// abandonedChainDiscoverer finds the latest Rekor entry ever published under
// this deployment's witness identity, independent of any local state.
type abandonedChainDiscoverer interface {
	AbandonedChainTip(context.Context) (model.AuditWitness, bool, error)
}

// witnessChainReset carries the operator's one-shot --accept-witness-chain-reset
// acknowledgment. The zero value disables the reset entirely and preserves the
// fail-closed startup behavior byte for byte.
type witnessChainReset struct {
	acknowledgedTip string
	chain           abandonedChainDiscoverer
	logger          *log.Logger
}

func (reset witnessChainReset) requested() bool { return reset.acknowledgedTip != "" }

func (reset witnessChainReset) printf(format string, args ...any) {
	if reset.logger != nil {
		reset.logger.Printf(format, args...)
	}
}

const (
	witnessChainResetActor        = "operator"
	witnessChainResetAction       = "witness-chain.reset-accepted"
	witnessChainResetResourceType = "audit-witness-chain"
)

type witnessChainResetDetails struct {
	AbandonedTipUUID         string `json:"abandonedTipUUID"`
	AbandonedTipLogIndex     int64  `json:"abandonedTipLogIndex"`
	AbandonedTipIntegratedAt string `json:"abandonedTipIntegratedAt"`
	AbandonedAuditID         int64  `json:"abandonedAuditID"`
	AbandonedAuditSHA256     string `json:"abandonedAuditSHA256"`
	AbandonedPreviousUUID    string `json:"abandonedPreviousUUID"`
	Acknowledgment           string `json:"acknowledgment"`
}

// verifyAcknowledgedTip proves that the operator-supplied UUID is exactly the
// most recent entry this witness identity ever published to Rekor. Anything
// else — an older entry, a foreign entry, a typo — fails closed.
func verifyAcknowledgedTip(ctx context.Context, reset witnessChainReset) (model.AuditWitness, bool, error) {
	abandoned, found, err := reset.chain.AbandonedChainTip(ctx)
	if err != nil {
		return model.AuditWitness{}, false, fmt.Errorf("discover abandoned witness chain tip: %w", err)
	}
	if !found {
		return model.AuditWitness{}, false, nil
	}
	if abandoned.UUID != reset.acknowledgedTip {
		return model.AuditWitness{}, false, fmt.Errorf(
			"refuse witness chain reset: --accept-witness-chain-reset=%s does not acknowledge the latest published Rekor witness %s (log index %d, integrated %s, audit head %d); look up the current tip and acknowledge it exactly",
			reset.acknowledgedTip, abandoned.UUID, abandoned.LogIndex,
			abandoned.IntegratedAt.UTC().Format(time.RFC3339), abandoned.AuditID,
		)
	}
	return abandoned, true, nil
}

// recordWitnessChainReset writes the acknowledgment as the first action of the
// new audit chain. The first published witness of the new chain commits a hash
// over this entry, permanently binding the new root to the abandoned tip and
// guaranteeing the new chain's genesis head differs from the deterministic
// empty-chain head the abandoned chain started from.
func recordWitnessChainReset(ctx context.Context, database *store.Store, abandoned model.AuditWitness) (model.AuditAction, error) {
	details, err := json.Marshal(witnessChainResetDetails{
		AbandonedTipUUID:         abandoned.UUID,
		AbandonedTipLogIndex:     abandoned.LogIndex,
		AbandonedTipIntegratedAt: abandoned.IntegratedAt.UTC().Format(time.RFC3339Nano),
		AbandonedAuditID:         abandoned.AuditID,
		AbandonedAuditSHA256:     hex.EncodeToString(abandoned.AuditSHA256),
		AbandonedPreviousUUID:    abandoned.PreviousUUID,
		Acknowledgment:           "--accept-witness-chain-reset",
	})
	if err != nil {
		return model.AuditAction{}, fmt.Errorf("encode witness chain reset acknowledgment: %w", err)
	}
	action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: witnessChainResetActor, Action: witnessChainResetAction,
		ResourceType: witnessChainResetResourceType, ResourceID: abandoned.UUID,
		Details: string(details),
	})
	if err != nil {
		return model.AuditAction{}, fmt.Errorf("record witness chain reset acknowledgment: %w", err)
	}
	if action.ID != 1 {
		return model.AuditAction{}, fmt.Errorf(
			"witness chain reset acknowledgment became audit action %d, want the first entry of the new chain", action.ID,
		)
	}
	return action, nil
}

func warnWitnessChainReset(reset witnessChainReset, abandoned model.AuditWitness, resumed bool) {
	mode := "accepting"
	if resumed {
		mode = "resuming acceptance of"
	}
	reset.printf(
		"WARNING: %s witness chain reset: permanently abandoning the existing Rekor witness chain at tip %s (log index %d, integrated %s, audit head %d/%s)",
		mode, abandoned.UUID, abandoned.LogIndex, abandoned.IntegratedAt.UTC().Format(time.RFC3339),
		abandoned.AuditID, hex.EncodeToString(abandoned.AuditSHA256),
	)
	reset.printf("WARNING: the abandoned chain remains publicly visible in the transparency log; this acknowledgment is recorded permanently as audit action 1 of the new chain")
	reset.printf("WARNING: witness chain reset is a one-shot acknowledgment: remove --accept-witness-chain-reset (helm value auditAnchor.acceptWitnessChainReset) after startup succeeds")
	reset.printf("WARNING: ensure no other Oberth instance uses the same SSH host key; a concurrent instance would continue the abandoned chain and fork the witness identity")
}

func openStartupDatabase(
	ctx context.Context,
	path string,
	continuity startupContinuity,
	reset witnessChainReset,
	adoption witnessGenesisAdoption,
	verifyExisting func(*store.Store) error,
) (*store.Store, error) {
	if continuity == nil || verifyExisting == nil {
		return nil, errors.New("startup database requires audit continuity and an existing-state verifier")
	}
	if reset.requested() && reset.chain == nil {
		return nil, errors.New("accepting a witness chain reset requires the external Rekor witness")
	}
	if adoption.requested() && reset.requested() {
		return nil, errors.New("witness genesis adoption and witness chain reset are mutually exclusive")
	}
	if adoption.requested() && adoption.chain == nil {
		return nil, errors.New("accepting a witness genesis requires the external Rekor witness")
	}
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		if reset.requested() {
			resumed, database, resumeErr := resumeWitnessChainReset(ctx, path, continuity, reset)
			if resumeErr != nil {
				return nil, resumeErr
			}
			if resumed {
				return database, nil
			}
		}
		if adoption.requested() {
			applied, database, adoptErr := adoptWitnessGenesis(ctx, path, continuity, adoption)
			if adoptErr != nil {
				return nil, adoptErr
			}
			if applied {
				return database, nil
			}
			// not applicable (already advanced / stale one-shot flag): the unchanged
			// fail-closed verification below decides.
		}
		inspection, openErr := store.InspectCurrent(ctx, path, store.Options{})
		if openErr != nil {
			if !errors.Is(openErr, store.ErrSchemaLegacyV1) {
				return nil, openErr
			}
			return migrateLegacyStartupDatabase(ctx, path, continuity, openErr)
		}
		verifyErr := verifyExisting(inspection)
		closeErr := inspection.Close()
		if err := errors.Join(verifyErr, closeErr); err != nil {
			return nil, err
		}
		return store.OpenCurrent(ctx, path, store.Options{})
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("inspect startup database: %w", err)
	}

	if adoption.requested() {
		return nil, errors.New("witness genesis adoption requires an existing database with audit history; a fresh install needs no adoption")
	}
	intents, err := continuity.Intents(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify empty genesis witness intents: %w", err)
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify empty genesis witness continuity: %w", err)
	}
	if len(intents) != 0 || len(pinned) != 0 {
		return nil, fmt.Errorf("refuse database genesis with rollback-external audit history: intents=%d completions=%d", len(intents), len(pinned))
	}
	if !reset.requested() {
		database, createErr := store.CreateGenesis(ctx, path, store.Options{})
		if createErr == nil {
			return database, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
		return adoptExistingGenesisDatabase(ctx, path, verifyExisting)
	}
	abandoned, found, err := verifyAcknowledgedTip(ctx, reset)
	if err != nil {
		return nil, err
	}
	if !found {
		reset.printf("accept-witness-chain-reset is set but the witness identity has no public Rekor history; continuing with a normal genesis")
		database, createErr := store.CreateGenesis(ctx, path, store.Options{})
		if createErr == nil {
			return database, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
		return adoptExistingGenesisDatabase(ctx, path, verifyExisting)
	}
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		database, err = adoptExistingGenesisDatabase(ctx, path, verifyExisting)
		if err != nil {
			return nil, err
		}
	}
	if _, err := recordWitnessChainReset(ctx, database, abandoned); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	warnWitnessChainReset(reset, abandoned, false)
	return database, nil
}

// adoptExistingGenesisDatabase handles a TOCTOU race where the genesis database
// file appeared between openStartupDatabase's lstat and store.CreateGenesis's
// exclusive create. If the existing file is a valid database that passes the
// startup verifier, it is adopted instead of crashing.
func adoptExistingGenesisDatabase(
	ctx context.Context,
	path string,
	verifyExisting func(*store.Store) error,
) (*store.Store, error) {
	inspection, err := store.InspectCurrent(ctx, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("adopt genesis database that appeared during startup: %w", err)
	}
	verifyErr := verifyExisting(inspection)
	closeErr := inspection.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return nil, fmt.Errorf("adopt genesis database that appeared during startup: %w", err)
	}
	return store.OpenCurrent(ctx, path, store.Options{})
}

// resumeWitnessChainReset covers the crash window between an accepted reset and
// its first rollback-external publication intent. Exactly two local states are
// reset-equivalent: an empty genesis database, and a database whose only audit
// action is a recorded acknowledgment of the same tip. Both require zero
// intents, zero pinned witnesses, and zero signed checkpoints; every other
// state defers to the unchanged fail-closed startup verification.
func resumeWitnessChainReset(
	ctx context.Context,
	path string,
	continuity startupContinuity,
	reset witnessChainReset,
) (bool, *store.Store, error) {
	inspection, err := store.InspectCurrent(ctx, path, store.Options{})
	if err != nil {
		return false, nil, nil // legacy or incompatible schema: the normal startup path decides
	}
	head, headErr := inspection.VerifyAuditState(ctx)
	_, anchorErr := inspection.LatestAuditAnchor(ctx)
	var recorded model.AuditAction
	recordedErr := errors.New("not inspected")
	if headErr == nil && head.ID == 1 {
		recorded, recordedErr = inspection.SoleAuditAction(ctx)
	}
	closeErr := inspection.Close()
	if headErr != nil || closeErr != nil {
		return false, nil, nil // let the normal path surface local verification errors
	}
	if !errors.Is(anchorErr, store.ErrNotFound) {
		return false, nil, nil // a signed checkpoint exists (or could not be read): the reset no longer applies
	}
	virginAcknowledgment := head.ID == 1 && recordedErr == nil &&
		recorded.Action == witnessChainResetAction && recorded.ResourceType == witnessChainResetResourceType
	if head.ID != 0 && !virginAcknowledgment {
		reset.printf("accept-witness-chain-reset is set but the local database already carries audit history; ignoring the one-shot flag")
		return false, nil, nil
	}
	intents, err := continuity.Intents(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("read witness intents for reset resume: %w", err)
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("read witness continuity for reset resume: %w", err)
	}
	if len(intents) != 0 || len(pinned) != 0 {
		if virginAcknowledgment {
			reset.printf("accept-witness-chain-reset acknowledgment already advanced to rollback-external continuity; ignoring the one-shot flag")
		}
		return false, nil, nil // rollback-external history exists: the fail-closed startup verification decides
	}
	if virginAcknowledgment && recorded.ResourceID != reset.acknowledgedTip {
		return false, nil, fmt.Errorf(
			"refuse witness chain reset resume: the local database already acknowledges tip %s but --accept-witness-chain-reset=%s was provided",
			recorded.ResourceID, reset.acknowledgedTip,
		)
	}
	abandoned, found, err := verifyAcknowledgedTip(ctx, reset)
	if err != nil {
		return false, nil, err
	}
	if !found {
		if !virginAcknowledgment {
			return false, nil, nil // no public history and no acknowledgment: normal genesis verification applies
		}
		return false, nil, errors.New(
			"refuse witness chain reset resume: the recorded acknowledgment names a Rekor witness but the identity search returned no public history",
		)
	}
	database, err := store.OpenCurrent(ctx, path, store.Options{})
	if err != nil {
		return false, nil, err
	}
	if !virginAcknowledgment {
		if _, err := recordWitnessChainReset(ctx, database, abandoned); err != nil {
			return false, nil, errors.Join(err, database.Close())
		}
	}
	warnWitnessChainReset(reset, abandoned, virginAcknowledgment)
	return true, database, nil
}

func migrateLegacyStartupDatabase(
	ctx context.Context,
	path string,
	continuity startupContinuity,
	exactSchemaErr error,
) (*store.Store, error) {
	legacyHead, err := store.InspectLegacyV1(ctx, path, store.Options{})
	if err != nil {
		return nil, errors.Join(exactSchemaErr, err)
	}
	intents, err := continuity.Intents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read legacy migration witness intents: %w", err)
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		return nil, fmt.Errorf("read legacy migration witness continuity: %w", err)
	}
	if len(pinned) != 0 || len(intents) > 1 {
		return nil, fmt.Errorf("refuse legacy schema migration with rollback-external audit history: intents=%d completions=%d",
			len(intents), len(pinned))
	}
	if len(intents) == 1 {
		intent := intents[0]
		if intent.Sequence != 1 || intent.AuditID != legacyHead.ID || intent.PreviousUUID != "" ||
			!bytes.Equal(intent.AuditSHA256, legacyHead.SHA256) {
			return nil, errors.New("refuse legacy schema migration with rollback-external audit history: existing intent does not bind the exact v1 audit head")
		}
	} else if legacyHead.ID > 0 {
		intent := model.AuditWitnessIntent{
			Sequence: 1, AuditID: legacyHead.ID, AuditSHA256: append([]byte(nil), legacyHead.SHA256...),
		}
		if err := continuity.Prepare(ctx, intent); err != nil {
			return nil, fmt.Errorf("persist immutable legacy migration witness intent: %w", err)
		}
	}

	migrated, err := store.MigrateLegacyV1(ctx, path, legacyHead, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("migrate verified legacy startup database: %w", err)
	}
	migratedHead, verifyErr := migrated.VerifyAuditState(ctx)
	if verifyErr == nil && (migratedHead.ID != legacyHead.ID || !bytes.Equal(migratedHead.SHA256, legacyHead.SHA256)) {
		verifyErr = errors.New("migrated audit head differs from the immutable legacy migration intent")
	}
	closeErr := migrated.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: migrated exact v1 state to ratified schema version 2; schema version 3 requires backup-and-replace before daemon startup",
		store.ErrSchemaIncompatible)
}

func runComponents(
	ctx context.Context,
	options serveOptions,
	scheduler *service.Scheduler,
	anchors *auditanchor.Manager,
	git *gitcache.Cache,
	sshService *sshserver.Server,
	httpsServer *http.Server,
	adminServer *http.Server,
	activateScheduler func(context.Context) error,
	accessReconciler *service.AccessReconciler,
	schedules *app.Schedules,
	logger *log.Logger,
) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var listenerConfig net.ListenConfig
	httpsRaw, err := listenerConfig.Listen(runCtx, "tcp", options.httpsListen)
	if err != nil {
		return fmt.Errorf("listen HTTPS: %w", err)
	}
	sshListener, err := listenerConfig.Listen(runCtx, "tcp", options.sshListen)
	if err != nil {
		_ = httpsRaw.Close()
		return fmt.Errorf("listen SSH: %w", err)
	}
	adminListener, err := listenAdminSocket(runCtx, defaultAdminSocket)
	if err != nil {
		_ = httpsRaw.Close()
		_ = sshListener.Close()
		return err
	}
	defer func() { _ = removeStaleAdminSocket(defaultAdminSocket) }()
	tlsListener := tls.NewListener(httpsRaw, httpsServer.TLSConfig)
	type componentResult struct {
		name string
		err  error
	}
	results := make(chan componentResult, 5)
	var workers sync.WaitGroup
	start := func(name string, operation func() error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := operation()
			if err == nil && runCtx.Err() == nil {
				err = errors.New("component stopped unexpectedly")
			}
			select {
			case results <- componentResult{name: name, err: err}:
			case <-runCtx.Done():
			}
		}()
	}
	start("scheduler", func() error {
		if activateScheduler != nil {
			if err := activateScheduler(runCtx); err != nil {
				return err
			}
		}
		return scheduler.Run(runCtx)
	})
	start("audit anchor", func() error { return anchors.Run(runCtx) })
	start("SSH", func() error { return sshService.Serve(runCtx, sshListener) })
	start("HTTPS", func() error {
		err := httpsServer.Serve(tlsListener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	start("admin", func() error {
		err := adminServer.Serve(adminListener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	})
	workers.Add(1)
	go func() {
		defer workers.Done()
		git.StartPeriodicGC(runCtx, options.gcInterval)
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		if schedules == nil {
			return
		}
		if err := schedules.Run(runCtx); err != nil && runCtx.Err() == nil {
			logger.Printf("schedule ticker stopped: %v", err)
		}
	}()
	if accessReconciler != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := accessReconciler.Watch(runCtx); err != nil && runCtx.Err() == nil {
				logger.Printf("secret access ConfigMap watcher stopped: %v", err)
			}
		}()
	}
	logger.Printf("ready for bootstrap: SSH %s, HTTPS %s", options.sshListen, options.httpsListen)

	var componentErr error
	select {
	case <-ctx.Done():
	case result := <-results:
		if result.err != nil {
			componentErr = fmt.Errorf("%s: %w", result.name, result.err)
		}
	}
	cancel()
	_ = sshListener.Close()
	_ = adminListener.Close()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	shutdownErr := httpsServer.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, httpsServer.Close())
	}
	adminShutdownErr := adminServer.Shutdown(shutdownCtx)
	if adminShutdownErr != nil {
		adminShutdownErr = errors.Join(adminShutdownErr, adminServer.Close())
	}
	workers.Wait()
	if ctx.Err() != nil {
		componentErr = nil
	}
	return errors.Join(componentErr, shutdownErr, adminShutdownErr)
}

func waitForAuditIntegrity(ctx context.Context, gate func(context.Context) error) error {
	if gate == nil {
		return errors.New("audit mutation gate is unavailable")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := gate(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// buildSecretStoreProbeClient constructs a dedicated secretstore.Client for the
// status-page login probe. The probe exercises SA → K8s-auth login → logout
// only; it never reads KV paths or transit keys.
func buildSecretStoreProbeClient(options serveOptions) (*secretstore.Client, error) {
	config := secretstore.Config{
		Address:                 options.secretStoreAddress,
		AllowInsecureHTTP:       options.secretStoreInsecureHTTP,
		AuthMountPath:           options.secretStoreAuthMount,
		Role:                    options.secretStoreRole,
		ServiceAccountTokenPath: options.secretStoreSAToken,
	}
	if options.secretStoreCACert != "" {
		caPEM, err := readBoundedFile(options.secretStoreCACert, 4<<20)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate: %w", err)
		}
		config.CACertPEM = caPEM
	}
	return secretstore.New(config)
}

// secretStoreStatus summarizes the operator-supplied OpenBao configuration for
// the read-only status view. Address, mount, and role are deployment
// configuration already visible in the pod spec; no credential material is
// ever included.
func secretStoreStatus(options serveOptions) *app.SecretStoreStatus {
	if options.secretStoreAddress == "" {
		return &app.SecretStoreStatus{Configured: false}
	}
	transport := "https"
	if strings.HasPrefix(options.secretStoreAddress, "http://") {
		transport = "insecure-http"
	}
	mount := options.secretStoreAuthMount
	if mount == "" {
		mount = secretstore.DefaultAuthMountPath
	}
	return &app.SecretStoreStatus{
		Configured: true,
		Address:    options.secretStoreAddress,
		AuthMount:  mount,
		Role:       options.secretStoreRole,
		Transport:  transport,
	}
}

// checkUpstreamSecretConfigured returns nil when both the upstream-key and
// known-hosts Secrets contain data matching the expected keys. This bridges
// the ~60s kubelet Secret-projection delay after `upstream add`: the Secret
// already holds the key before the projected volume mount catches up.
func checkUpstreamSecretConfigured(
	ctx context.Context,
	kube kubernetes.Interface,
	namespace, upstreamKeyPath, knownHostsPath string,
	registered []model.Upstream,
) error {
	secretCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	keySecret, err := kube.CoreV1().Secrets(namespace).Get(secretCtx, defaultUpstreamKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	hostsSecret, err := kube.CoreV1().Secrets(namespace).Get(secretCtx, defaultUpstreamKnownHostsSecret, metav1.GetOptions{})
	if err != nil {
		return err
	}

	hostsKey := filepath.Base(knownHostsPath)
	if len(hostsSecret.Data[hostsKey]) == 0 {
		return errors.New("known_hosts Secret has no data for the expected key")
	}

	// Check the shared upstream key.
	keyFile := filepath.Base(upstreamKeyPath)
	if len(keySecret.Data[keyFile]) > 0 {
		return nil
	}

	// Check per-upstream dedicated keys.
	for _, upstream := range registered {
		if upstream.Kind != "ssh" || upstream.KeyName == "" || !app.ValidUpstreamKeyName(upstream.KeyName) {
			continue
		}
		if len(keySecret.Data[upstream.KeyName]) > 0 {
			return nil
		}
	}

	return errors.New("upstream key Secret has no matching data key")
}

func loadTSARoots(path string) (*x509.CertPool, error) {
	if path == "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system TSA trust roots: %w", err)
		}
		return roots, nil
	}
	body, err := readBoundedFile(path, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read TSA trust roots: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(body) {
		return nil, errors.New("load TSA trust roots: PEM file contains no certificates")
	}
	return roots, nil
}

// loadOptionalCACert loads a PEM-encoded CA certificate bundle for pinning an
// HTTPS connection's TLS trust anchors. Returns nil when the path is empty,
// indicating the system trust store should be used.
func loadOptionalCACert(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	body, err := readBoundedFile(path, 4<<20)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, fmt.Errorf("%s: PEM file contains no certificates", path)
	}
	return pool, nil
}

// loadRekorPublicKey loads an optional PEM-encoded Rekor log public key. A nil
// result preserves the default Sigstore TUF trust-root path.
func loadRekorPublicKey(path string) (crypto.PublicKey, error) {
	if path == "" {
		return nil, nil
	}
	body, err := readBoundedFile(path, 1<<20)
	if err != nil {
		return nil, err
	}
	publicKey, err := cryptoutils.UnmarshalPEMToPublicKey(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return publicKey, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	// #nosec G304 G703 -- every caller supplies an operator- or server-controlled
	// path: serve command-line flags (SSH host key, known-hosts, CA certificates)
	// and the server-injected VAULT_CACERT constant that `secretstore exec` reads.
	// None is repository-authored; the taint gosec traces from os.Getenv
	// originates in Oberth's own injected environment, not an untrusted source.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return nil, fmt.Errorf("%s must be a non-empty regular file no larger than %d bytes", path, maximum)
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, fmt.Errorf("%s grew beyond %d bytes while being read", path, maximum)
	}
	return body, nil
}

// buildPerUpstreamSSHCommand loads all upstreams from the database and builds
// an SSH config file mapping each upstream host with a dedicated key to its
// projected identity file on the Secret volume mount, with a fallback block
// (per-host names negated) using the shared --upstream-key for everything
// else. Private key material is never read or copied here: identity files are
// referenced in place on the kubelet-managed tmpfs mount, so no key bytes
// ever reach the container's writable layer. The config file itself contains
// only hostnames and paths. When no upstream has a dedicated key the result
// is the legacy single-key command, byte-identical in behavior.
func buildPerUpstreamSSHCommand(ctx context.Context, database *store.Store, options serveOptions, configPath string, logger *log.Logger) (string, error) {
	registeredUpstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return "", fmt.Errorf("list upstreams for SSH config: %w", err)
	}
	keyDirectory := filepath.Dir(options.upstreamKey)
	var entries []app.SSHConfigEntry
	for _, upstream := range registeredUpstreams {
		if upstream.KeyName == "" || upstream.Kind != "ssh" {
			continue
		}
		// Path construction treats database rows as untrusted input even
		// though registration validates names: a key name must stay a single
		// clean path element before it is joined onto the key directory.
		if !app.ValidUpstreamKeyName(upstream.KeyName) {
			logger.Printf("WARNING: upstream %s has malformed key name; using the shared fallback key", upstream.Name)
			continue
		}
		host, port := app.UpstreamSSHHost(upstream.BaseURL)
		if host == "" {
			continue
		}
		identityFile := filepath.Join(keyDirectory, upstream.KeyName)
		if _, statErr := os.Stat(identityFile); statErr != nil {
			logger.Printf("WARNING: per-upstream key %s for upstream %s is not projected yet: %v; using the shared fallback key", upstream.KeyName, upstream.Name, statErr)
			continue
		}
		entries = append(entries, app.SSHConfigEntry{
			Host:         host,
			Port:         port,
			IdentityFile: identityFile,
		})
		logger.Printf("per-upstream SSH key: %s -> %s (data key %s)", upstream.Name, host, upstream.KeyName)
	}
	if len(entries) == 0 {
		return app.GitSSHCommandPaths(options.upstreamKey, options.knownHosts)
	}
	configBody, err := app.BuildSSHConfig(entries, options.upstreamKey, options.knownHosts)
	if err != nil {
		return "", fmt.Errorf("build per-upstream SSH config: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		return "", fmt.Errorf("write per-upstream SSH config: %w", err)
	}
	return app.GitSSHCommandFromConfig(configPath)
}

func pathWithin(root, path string) bool { return gitoid.StrictPathWithin(root, path) }

// classifyViewError maps service-layer sentinel errors to HTTP status codes.
// Wired into the API server at construction so the api package does not import
// service or store.
func classifyViewError(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrInvalidInput),
		errors.Is(err, runlog.ErrInvalidPattern):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, service.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, store.ErrAmbiguous),
		errors.Is(err, service.ErrAmbiguousRepository):
		return http.StatusConflict, err.Error()
	case errors.Is(err, store.ErrInvalidState):
		// State-based refusals (in-flight runs, immutable history, terminal
		// records) are actionable answers, not server faults: keep the message
		// instead of collapsing it into an opaque internal-error reference.
		return http.StatusConflict, err.Error()
	case errors.Is(err, service.ErrUnavailable):
		return http.StatusServiceUnavailable, "service unavailable"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

const (
	defaultScheduleMinInterval  = 15 * time.Minute
	defaultScheduleMaxEntries   = 8
	defaultArtifactsLimitBytes  = 256 << 20
	defaultArtifactsBudgetBytes = 4 << 30
)
