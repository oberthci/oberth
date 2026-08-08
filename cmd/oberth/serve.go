package main

import (
	"bytes"
	"context"
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
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/job"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/store"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type stringList []string

var (
	runnerDigestPattern          = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	witnessChainResetUUIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{64}|[0-9a-f]{80})$`)
)

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
	runnerImage             string
	pvcName                 string
	maxConcurrent           int
	ciCacheRoot             string
	releaseCacheRoot        string
	stepTimeout             string
	jobTimeout              time.Duration
	jobTTL                  int
	cpuRequest              string
	memoryRequest           string
	cpuLimit                string
	memoryLimit             string
	ephemeralStorageRequest string
	ephemeralStorageLimit   string
	releaseSecrets          stringList
	secretStoreAddress      string
	secretStoreAuthMount    string
	secretStoreRole         string
	secretStoreCACert       string
	secretStoreSAToken      string
	secretStorePaths        stringList
	secretStoreInsecureHTTP bool
	gitCommandTimeout       time.Duration
	gcInterval              time.Duration
	auditTSAURL             string
	auditRekorURL           string
	auditTSARoots           string
	auditAnchorInterval     time.Duration
	auditAnchorMaxAge       time.Duration
	acceptWitnessChainReset string
}

func runServe(ctx context.Context, arguments []string, output io.Writer) error {
	options, err := parseServeOptions(arguments)
	if err != nil {
		return err
	}
	logger := log.New(output, "oberth: ", log.LstdFlags|log.LUTC)
	return serve(ctx, options, logger)
}

func parseServeOptions(arguments []string) (serveOptions, error) {
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
	flags.StringVar(&options.runnerImage, "runner-image", "", "digest-pinned runner image")
	flags.StringVar(&options.pvcName, "pvc", "oberth", "PVC name")
	flags.IntVar(&options.maxConcurrent, "max-concurrent-jobs", 3, "maximum concurrent Jobs")
	flags.StringVar(&options.ciCacheRoot, "ci-cache-root", "/var/cache/oberth/ci", "CI host cache root")
	flags.StringVar(&options.releaseCacheRoot, "release-cache-root", "/var/cache/oberth/release", "release host cache root")
	flags.StringVar(&options.stepTimeout, "step-timeout", "30m", "default step timeout")
	flags.DurationVar(&options.jobTimeout, "job-timeout", 12*time.Hour, "outer Job timeout")
	flags.IntVar(&options.jobTTL, "job-ttl", 3600, "finished Job TTL in seconds")
	flags.StringVar(&options.cpuRequest, "job-cpu-request", "1", "Job CPU request")
	flags.StringVar(&options.memoryRequest, "job-memory-request", "2Gi", "Job memory request")
	flags.StringVar(&options.cpuLimit, "job-cpu-limit", "3", "Job CPU limit")
	flags.StringVar(&options.memoryLimit, "job-memory-limit", "4Gi", "Job memory limit")
	flags.StringVar(&options.ephemeralStorageRequest, "job-ephemeral-storage-request", "1Gi", "Job ephemeral-storage request")
	flags.StringVar(&options.ephemeralStorageLimit, "job-ephemeral-storage-limit", "8Gi", "Job ephemeral-storage limit and tmp ceiling")
	flags.Var(&options.releaseSecrets, "release-secret", "release Secret name (repeatable)")
	flags.StringVar(&options.secretStoreAddress, "secretstore-address", "", "OpenBao API base URL for store-sourced release secrets (HTTPS)")
	flags.StringVar(&options.secretStoreAuthMount, "secretstore-k8s-auth-mount", secretstore.DefaultAuthMountPath, "OpenBao Kubernetes auth mount path")
	flags.StringVar(&options.secretStoreRole, "secretstore-role", "", "OpenBao Kubernetes auth role bound to the Oberth ServiceAccount")
	flags.StringVar(&options.secretStoreCACert, "secretstore-ca-cert", "", "optional PEM file pinning the OpenBao TLS trust anchors")
	flags.StringVar(&options.secretStoreSAToken, "secretstore-sa-token", "", "optional ServiceAccount token path override for the OpenBao login")
	flags.Var(&options.secretStorePaths, "secretstore-path", "allowlisted OpenBao KV path (repeatable)")
	flags.BoolVar(&options.secretStoreInsecureHTTP, "secretstore-insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address")
	flags.DurationVar(&options.gitCommandTimeout, "git-timeout", 10*time.Minute, "Git command timeout")
	flags.DurationVar(&options.gcInterval, "git-gc-interval", 24*time.Hour, "periodic Git GC interval")
	flags.StringVar(&options.auditTSAURL, "audit-tsa-url", "https://timestamp.sectigo.com/rfc3161", "RFC 3161 timestamp authority HTTPS URL")
	flags.StringVar(&options.auditRekorURL, "audit-rekor-url", "https://rekor.sigstore.dev", "Rekor transparency log URL")
	flags.StringVar(&options.auditTSARoots, "audit-tsa-roots", "", "optional PEM file containing pinned TSA trust roots")
	flags.DurationVar(&options.auditAnchorInterval, "audit-anchor-interval", 10*time.Minute, "external audit checkpoint interval")
	flags.DurationVar(&options.auditAnchorMaxAge, "audit-anchor-max-age", 30*time.Minute, "maximum checkpoint age allowed by readiness")
	flags.StringVar(&options.acceptWitnessChainReset, "accept-witness-chain-reset", "",
		"one-shot acknowledgment: exact Rekor UUID of the latest published witness before a fresh genesis permanently abandons that chain")
	if err := flags.Parse(arguments); err != nil {
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
	if options.maxConcurrent <= 0 || options.maxConcurrent > 64 || options.jobTimeout <= 0 || options.jobTTL <= 0 || options.jobTTL > 1<<31-1 || options.gitCommandTimeout <= 0 || options.gcInterval <= 0 {
		return errors.New("serve: concurrency, timeout, TTL, and GC values must be positive")
	}
	if !runnerDigestPattern.MatchString(options.runnerImage) || strings.TrimSpace(options.namespace) == "" || strings.TrimSpace(options.pvcName) == "" {
		return errors.New("serve: runner image, namespace, and PVC are required")
	}
	if !validAuditHTTPSURL(options.auditTSAURL) {
		return errors.New("serve: audit TSA URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if !validAuditHTTPSURL(options.auditRekorURL) {
		return errors.New("serve: audit Rekor URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if options.auditAnchorInterval <= 0 || options.auditAnchorMaxAge < options.auditAnchorInterval {
		return errors.New("serve: audit anchor interval must be positive and max age must be at least the interval")
	}
	if options.auditTSARoots != "" && (!filepath.IsAbs(options.auditTSARoots) || filepath.Clean(options.auditTSARoots) != options.auditTSARoots) {
		return errors.New("serve: audit TSA roots must be a clean absolute path")
	}
	if options.acceptWitnessChainReset != "" && !witnessChainResetUUIDPattern.MatchString(options.acceptWitnessChainReset) {
		return errors.New("serve: accept-witness-chain-reset must be the exact Rekor UUID (64 or 80 lowercase hex characters) of the latest published witness")
	}
	return validateSecretStoreServeOptions(options)
}

func validateSecretStoreServeOptions(options serveOptions) error {
	if options.secretStoreAddress == "" {
		if options.secretStoreRole != "" || options.secretStoreCACert != "" || options.secretStoreSAToken != "" ||
			len(options.secretStorePaths) != 0 || options.secretStoreInsecureHTTP || options.secretStoreAuthMount != secretstore.DefaultAuthMountPath {
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
	return nil
}

func validAuditHTTPSURL(raw string) bool {
	if !strings.HasPrefix(raw, "https://") || strings.Contains(raw, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && parsed.Host != "" && parsed.Scheme == "https" && parsed.User == nil && parsed.Fragment == ""
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
	tsaRoots, err := loadTSARoots(options.auditTSARoots)
	if err != nil {
		return err
	}
	tsaClient, err := auditanchor.NewClient(auditanchor.ClientConfig{Endpoint: options.auditTSAURL, Roots: tsaRoots})
	if err != nil {
		return err
	}
	rekorWitness, err := auditanchor.NewRekorWitness(ctx, options.auditRekorURL, hostKey)
	if err != nil {
		return err
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
	database, err := openStartupDatabase(ctx, options.database, continuity, reset, func(inspection *store.Store) error {
		verifier, managerErr := auditanchor.NewManager(auditanchor.ManagerConfig{
			Store: inspection, Authority: tsaClient, Witness: rekorWitness, Continuity: continuity,
			Interval: options.auditAnchorInterval, MaxAge: options.auditAnchorMaxAge, Logger: logger,
		})
		if managerErr != nil {
			return managerErr
		}
		return verifier.VerifyStartup(ctx)
	})
	if err != nil {
		return err
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
	anchors, err := auditanchor.NewManager(auditanchor.ManagerConfig{
		Store: database, Authority: tsaClient, Witness: rekorWitness, Continuity: continuity, Interval: options.auditAnchorInterval,
		MaxAge: options.auditAnchorMaxAge, Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := anchors.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize verified audit continuity: %w", err)
	}

	sshCommand, err := app.GitSSHCommandPaths(options.upstreamKey, options.knownHosts)
	if err != nil {
		return err
	}
	upstreams := app.Upstreams{Catalog: database}
	git, err := gitcache.New(gitcache.Config{
		Root: filepath.Join(options.dataRoot, "git"), Upstream: upstreams.Remote,
		CommandTimeout: options.gitCommandTimeout,
		Env:            map[string]string{"GIT_SSH_COMMAND": sshCommand, "GIT_SSH_VARIANT": "ssh"},
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	logs, err := runlog.Open(filepath.Join(options.dataRoot, "logs"))
	if err != nil {
		return err
	}
	storeFetcher, execStreamer, err := secretStoreDelivery(options, restConfig, kube)
	if err != nil {
		return err
	}
	jobController, err := job.NewController(kube, job.Config{
		Namespace: options.namespace, Image: options.runnerImage, PullPolicy: corev1.PullIfNotPresent,
		PVCName: options.pvcName, CICacheRoot: options.ciCacheRoot, ReleaseCacheRoot: options.releaseCacheRoot,
		ProvisionCacheDirs: true,
		ReleaseSecrets:     options.releaseSecrets, CPURequest: options.cpuRequest, MemoryRequest: options.memoryRequest,
		CPULimit: options.cpuLimit, MemoryLimit: options.memoryLimit,
		EphemeralStorageRequest: options.ephemeralStorageRequest, EphemeralStorageLimit: options.ephemeralStorageLimit,
		StepTimeout: options.stepTimeout,
		JobTimeout:  options.jobTimeout,
		// #nosec G115 -- validateServeOptions bounds jobTTL to a positive int32.
		TTLSeconds:       int32(options.jobTTL),
		SecretStorePaths: options.secretStorePaths,
	}, nil, storeFetcher, execStreamer)
	if err != nil {
		return err
	}
	jobs, err := app.NewJobs(jobController, filepath.Join(options.dataRoot, "work"), database)
	if err != nil {
		return err
	}
	signals := service.NewSignals()
	scheduler, err := service.NewScheduler(service.SchedulerConfig{
		Store: database, Git: git, Logs: logs, Jobs: jobs, ReleaseJobs: jobs, Auditor: database,
		Signals: signals, WorkspaceRoot: filepath.Join(options.dataRoot, "work"), MaxConcurrent: options.maxConcurrent,
		MutationGate: anchors.AllowMutation,
	})
	if err != nil {
		return err
	}
	health := app.Health{Store: database, Audit: anchors.Ready, Configured: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := app.GitSSHCommand(options.upstreamKey, options.knownHosts)
		return err
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
		privateKey, err := readBoundedFile(options.upstreamKey, 1<<20)
		if err != nil {
			return fmt.Errorf("read upstream identity: %w", err)
		}
		knownHosts, err := readBoundedFile(options.knownHosts, 1<<20)
		if err != nil {
			return fmt.Errorf("read upstream host pins: %w", err)
		}
		return app.ProbeSSHCapability(ctx, upstream.BaseURL, privateKey, knownHosts)
	}, Cluster: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := kube.Discovery().ServerVersion()
		return err
	}}
	controlAPI, err := service.NewAPI(service.APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, PromotionRuns: database, Enqueues: scheduler, Git: git,
		Logs: logs, Auditor: database, Health: health, Signals: signals,
		MutationGate:           anchors.AllowMutation,
		PromotionWorkspaceRoot: filepath.Join(options.dataRoot, "work"),
	})
	if err != nil {
		return err
	}
	authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: database})
	if err != nil {
		return err
	}
	httpAPI, err := api.New(authenticator, controlAPI, controlAPI, version)
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
		OnUpdate: receiveHandler, Logger: logger, MutationGate: anchors.AllowMutation,
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
	return runComponents(ctx, options, scheduler, anchors, git, sshService, httpsServer, newAdminGateServer(anchors.AllowMutation, options.database), activateScheduler, logger)
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
	verifyExisting func(*store.Store) error,
) (*store.Store, error) {
	if continuity == nil || verifyExisting == nil {
		return nil, errors.New("startup database requires audit continuity and an existing-state verifier")
	}
	if reset.requested() && reset.chain == nil {
		return nil, errors.New("accepting a witness chain reset requires the external Rekor witness")
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
		inspection, openErr := store.InspectCurrent(ctx, path, store.Options{})
		if openErr != nil {
			if !errors.Is(openErr, store.ErrSchemaLegacyV1) {
				return nil, openErr
			}
			return migrateLegacyStartupDatabase(ctx, path, continuity, verifyExisting, openErr)
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
		return store.CreateGenesis(ctx, path, store.Options{})
	}
	abandoned, found, err := verifyAcknowledgedTip(ctx, reset)
	if err != nil {
		return nil, err
	}
	if !found {
		reset.printf("accept-witness-chain-reset is set but the witness identity has no public Rekor history; continuing with a normal genesis")
		return store.CreateGenesis(ctx, path, store.Options{})
	}
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		return nil, err
	}
	if _, err := recordWitnessChainReset(ctx, database, abandoned); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	warnWitnessChainReset(reset, abandoned, false)
	return database, nil
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
	verifyExisting func(*store.Store) error,
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

	inspection, err := store.InspectCurrent(ctx, path, store.Options{})
	if err != nil {
		return nil, err
	}
	verifyErr = verifyExisting(inspection)
	closeErr = inspection.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return nil, err
	}
	return store.OpenCurrent(ctx, path, store.Options{})
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

// secretStoreDelivery builds the fail-closed secret store fetch and exec-delivery pair.
// Both exist only when the operator configures a secret store address; a repository
// declaring SecretStoreSecrets against an unconfigured server fails release
// admission with a clear error instead of degrading.
func secretStoreDelivery(options serveOptions, restConfig *rest.Config, kube kubernetes.Interface) (job.SecretStoreFetcher, job.ExecStreamer, error) {
	if options.secretStoreAddress == "" {
		return nil, nil, nil
	}
	var caPEM []byte
	if options.secretStoreCACert != "" {
		body, err := readBoundedFile(options.secretStoreCACert, 4<<20)
		if err != nil {
			return nil, nil, fmt.Errorf("read secret store CA certificate: %w", err)
		}
		caPEM = body
	}
	fetcher, err := secretstore.New(secretstore.Config{
		Address:                 options.secretStoreAddress,
		AllowInsecureHTTP:       options.secretStoreInsecureHTTP,
		AuthMountPath:           options.secretStoreAuthMount,
		Role:                    options.secretStoreRole,
		CACertPEM:               caPEM,
		ServiceAccountTokenPath: options.secretStoreSAToken,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("configure secret store secret source: %w", err)
	}
	return fetcher, newExecStreamer(restConfig, kube), nil
}

// newExecStreamer delivers secret store payloads over the Kubernetes exec
// subresource. The payload transits the API server and kubelet streams in
// memory only; nothing is persisted, and stream bodies never enter the
// Kubernetes audit log.
func newExecStreamer(restConfig *rest.Config, kube kubernetes.Interface) job.ExecStreamer {
	return func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, output io.Writer) error {
		request := kube.CoreV1().RESTClient().Post().
			Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container,
				Command:   command,
				Stdin:     true,
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
		return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: output, Stderr: output})
	}
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

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	// #nosec G304 -- the operator explicitly supplies this in-pod identity path.
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

func pathWithin(root, path string) bool { return gitoid.StrictPathWithin(root, path) }
