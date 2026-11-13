package installer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"

	"github.com/oberthci/oberth/internal/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// CheckResult is a stable public check. Diagnostics never include subprocess output.
type CheckResult struct {
	Warning         bool   `json:"warning,omitempty"`
	Evidence        string `json:"evidence,omitempty"`
	DeployPublicKey string `json:"deploy_public_key,omitempty"`
	Code            string `json:"code"`
	OK              bool   `json:"ok"`
	Remedy          string `json:"remedy,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type InstallReview struct {
	ArgoSecurity             []ArgoSecurityReview         `json:"argo_security,omitempty"`
	ArgoParallelism          string                       `json:"argo_parallelism"`
	ArgoNamespaceParallelism string                       `json:"argo_namespace_parallelism"`
	ArgoExecutorResources    *ArgoExecutorResourcesReview `json:"argo_executor_resources,omitempty"`
	UpstreamName             string                       `json:"upstream_name,omitempty"`
	ClientRequirements       []string                     `json:"client_requirements,omitempty"`
	NetworkPolicy            string                       `json:"network_policy"`
	ReissueServerCA          bool                         `json:"reissue_server_ca"`
	Context                  string                       `json:"context"`
	Namespace                string                       `json:"namespace"`
	ArgoNamespace            string                       `json:"argo_namespace"`
	ProductionStore          bool                         `json:"production_store"`
	DevStore                 bool                         `json:"dev_store"`
	ClientHost               string                       `json:"client_host"`
	AdminIdentity            string                       `json:"admin_identity"`
	DNSNames                 []string                     `json:"tls_dns_names"`
	IPs                      []string                     `json:"tls_ips"`
}

func Review(cfg Config) InstallReview {
	name, _, _ := cfg.primaryUpstream()
	dns, ips := slices.Clone(cfg.TLSExtraDNSNames), slices.Clone(cfg.TLSExtraIPs)
	if cfg.ClientHost != "" {
		dns, ips = CertificateNamesForClientHost(cfg.ClientHost, dns, ips)
	}
	return InstallReview{ArgoSecurity: cfg.argoSecurityReview, ArgoParallelism: argoReviewLimit(cfg, cfg.ArgoParallelism, "parallelism", DefaultArgoParallelism), ArgoNamespaceParallelism: argoReviewLimit(cfg, cfg.ArgoNamespaceParallelism, "namespaceParallelism", DefaultArgoNamespaceParallelism), ArgoExecutorResources: argoExecutorResourcesForReview(cfg), UpstreamName: name, ClientRequirements: clientRequirements(cfg), NetworkPolicy: effectiveNetworkPolicy(cfg), ReissueServerCA: cfg.ReissueServerCA, Context: cfg.ContextName, Namespace: cfg.Namespace, ArgoNamespace: cfg.ArgoNamespace, ProductionStore: cfg.InstallSecretStore, DevStore: cfg.InstallSecretStoreDev, ClientHost: cfg.ClientHost, AdminIdentity: cfg.UplinkIdentity, DNSNames: dns, IPs: ips}
}

func PrintReviewJSON(w io.Writer, cfg Config) error {
	return json.NewEncoder(w).Encode(struct {
		Version              int           `json:"version"`
		Review               InstallReview `json:"review"`
		ConfirmationRequired bool          `json:"confirmation_required"`
	}{1, Review(cfg), true})
}

type InstallClientAccess struct {
	Authentication string `json:"authentication"`
	TokenRequired  bool   `json:"token_required"`
	NextStep       string `json:"next_step"`
}

func certificateClientAccess() *InstallClientAccess {
	return &InstallClientAccess{Authentication: "mutual_tls", TokenRequired: false,
		NextStep: "Run /oberth:connect with the server CA pin and local client certificate/key paths."}
}

type InstallResult struct {
	ClientAccess  *InstallClientAccess `json:"client_access,omitempty"`
	Upstream      *InstallUpstream     `json:"upstream,omitempty"`
	Cluster       *ClusterIdentity     `json:"cluster,omitempty"`
	SecretsValues []string             `json:"secrets_values,omitempty"`
	Review        *InstallReview       `json:"review,omitempty"`
	Version       int                  `json:"version"`
	OK            bool                 `json:"ok"`
	Context       string               `json:"context"`
	ServerURL     string               `json:"server_url,omitempty"`
	MCPURL        string               `json:"mcp_url,omitempty"`
	GitURL        string               `json:"git_url,omitempty"`
	CAPin         string               `json:"ca_sha256,omitempty"`
	FirstUplink   string               `json:"first_uplink,omitempty"`
	SecretsFile   string               `json:"secrets_file,omitempty"`
	SecretsSHA256 string               `json:"secrets_sha256,omitempty"`
	Checks        []CheckResult        `json:"checks"`
}

func (r *InstallResult) check(code string, ok bool, remedy string) {
	if ok {
		remedy = ""
	} else {
		r.OK = false
	}
	r.Checks = append(r.Checks, CheckResult{Code: code, OK: ok, Remedy: remedy})
}

// PrintReview deliberately enumerates only non-secret install choices.
func PrintReview(w io.Writer, cfg Config) { printReview(w, Review(cfg)) }
func printReview(w io.Writer, review InstallReview) {
	printArgoSecurityReview(w, review.ArgoSecurity)
	_, _ = fmt.Fprintf(w, "Review install: context %q, namespace %q, pipeline namespace %q\nSecret store: production=%t dev=%t; client address=%q; admin identity=%q\nTLS DNS names: %v; IP addresses: %v\n", review.Context, review.Namespace, review.ArgoNamespace, review.ProductionStore, review.DevStore, review.ClientHost, review.AdminIdentity, review.DNSNames, review.IPs)
	_, _ = fmt.Fprintf(w, "Pipeline NetworkPolicy: %s (effective install choice)\n", review.NetworkPolicy)
	_, _ = fmt.Fprintln(w, "NetworkPolicy enforcement check runs first at install, after approval, using temporary probe Pods and their scratch namespace; fresh installs refuse unless enforcement is measured.")
	printArgoExecutorResources(w, review.ArgoExecutorResources)
	_, _ = fmt.Fprintf(w, "Argo controller.parallelism: %s; controller.namespaceParallelism: %s (0 means unlimited)\n", review.ArgoParallelism, review.ArgoNamespaceParallelism)
	for _, requirement := range review.ClientRequirements {
		_, _ = fmt.Fprintln(w, requirement)
	}
	if review.UpstreamName != "" {
		_, _ = fmt.Fprintf(w, "Upstream name: %s (change with --upstream-name before installing)\n", review.UpstreamName)
	}
	printPolicyWarning(w, review.NetworkPolicy)
}

// VerifyInstall reads cluster state and verifies the advertised TLS endpoint
// against the CA obtained over the authenticated Kubernetes connection.
func VerifyInstall(ctx context.Context, cfg Config, host InstallDeps) InstallResult {
	return verifyInstall(ctx, cfg, host, false)
}

func verifyInstall(ctx context.Context, cfg Config, host InstallDeps, maintenance bool) InstallResult {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	progress := host.Output
	host.Output = io.Discard
	host = host.withDefaults()
	r := InstallResult{ClientAccess: certificateClientAccess(), Version: 1, OK: true, Context: cfg.ContextName, Checks: []CheckResult{}}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.KubeconfigPath != "" {
		host.LoadKubeConfig = func(name string) (kubernetes.Interface, *rest.Config, string, error) {
			rules := kubeconfig.Rules("")
			rules.ExplicitPath = cfg.KubeconfigPath
			return loadKubeConfigWithRules(rules, name, "", io.Discard)
		}
	}
	if host.KubeContexts != nil {
		err := RequireContextChoice("verify", cfg.ContextName, cfg.KubeconfigPath, host.KubeContexts)
		r.check("context_choice", err == nil, "Select the intended context with --context.")
		if err != nil {
			return r
		}
	}
	kube, config, selected, err := host.LoadKubeConfig(cfg.ContextName)
	r.check("kubeconfig", err == nil, "Select a readable kubeconfig and context with working credentials.")
	if err != nil {
		return r
	}
	r.Context = selected
	run := host.RunCommand
	if cfg.KubeconfigPath != "" {
		run = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if name == "kubectl" {
				args = append([]string{"--kubeconfig", cfg.KubeconfigPath}, args...)
			}
			return host.RunCommand(ctx, input, name, args...)
		}
	}
	deps := Deps{KubeClient: kube, RestConfig: config, ContextName: selected, ClientHost: cfg.ClientHost, Output: io.Discard, RunCommand: run, ProbeNetworkPolicy: host.ProbeNetworkPolicy}
	identity := publicClusterIdentity(cfg, config, selected)
	if !checkClusterTrust(config, identity, &r) {
		r.Cluster = &identity
		return r
	}
	// Verification reads installed releases afresh, not pre-apply snapshots.
	cfg.existingValuesRead, cfg.existingRelease, cfg.liveInstallValues = false, false, nil
	cfg.argoReleaseRead, cfg.argoValuesRead, cfg.existingArgoRelease, cfg.liveArgoValues = false, false, false, nil
	cfg.argoExecutorResources = nil
	if maintenance {
		if err := readInstallValues(ctx, &cfg, installPolicyDeps(host, cfg, selected)); err != nil || !cfg.existingRelease {
			r.check("installed_values", false, "Restore read access to the existing Helm release.")
			return r
		}
		cfg.NetworkPolicy = "auto"
	} else if err := resolveInstalledPolicy(ctx, &cfg, installPolicyDeps(host, cfg, selected)); err != nil {
		r.check("network_policy", false, "Installed NetworkPolicy setting is unverified; restore read access to the installed Helm release values.")
		r.Checks[len(r.Checks)-1].Reason = "unverified"
		return r
	}
	if err := refuseRemovedReleaseValues(cfg.liveInstallValues, "oberth install"); err != nil {
		// Verification still describes the installed release. Obsolete values
		// require an upgrade before the next write, not a failed read check.
		r.Checks = append(r.Checks, CheckResult{Code: "installed_values", OK: true, Warning: true, Remedy: err.Error()})
	}

	if err := readArgoReviewValues(ctx, &cfg, installPolicyDeps(host, cfg, selected)); err != nil {
		r.check("argo_values", false, argoValuesRemedy)
		r.Cluster = &identity
		return r
	}
	if err := resolveArgoExecutorResources(ctx, &cfg, installPolicyDeps(host, cfg, selected)); err != nil {
		r.check("argo_executor_resources", false, "Read the stored argo-workflows values so operator executor.resources can be preserved.")
		r.Cluster = &identity
		return r
	}
	preflightDeps := deps
	preflightDeps.Output = progress
	preflightDeps.RunCommand = host.RunCommand
	inspectCluster(ctx, &cfg, preflightDeps, &identity, &r, maintenance, host.VerifyProbe)
	if !maintenance && !host.VerifyProbe {
		for i := range r.Checks {
			check := &r.Checks[i]
			if check.Code == "network_policy" && check.Warning && check.Reason == "unverified" {
				check.OK, check.Warning, check.Reason, check.Remedy = true, false, "not-retested", ""
				check.Evidence = "NetworkPolicy enforcement was measured at install; this verification did not re-test it. Use oberth install --verify --probe to run a live measurement."
			}
		}
	}
	review := Review(cfg)
	r.Review = &review
	r.Cluster = &identity
	service, serviceErr := kube.CoreV1().Services(cfg.Namespace).Get(ctx, "oberth", metav1.GetOptions{})
	deps.ClientServiceType = cfg.ServiceType
	r.check("service", serviceErr == nil, "Restore the Oberth Service and Kubernetes read permissions.")
	deps, endpointErr := withInstalledServiceEndpoint(service, cfg.Namespace, deps)
	r.check("client_endpoint", endpointErr == nil, "Correct the advertised clientEndpoint values in the Oberth Helm release.")
	if endpointErr != nil {
		return r
	}
	deps = withClientEndpoint(cfg, deps)
	ssh, https := clientPorts(deps)
	address := clientReachableHost(deps)
	r.ServerURL = "https://" + net.JoinHostPort(address, https)
	r.MCPURL = r.ServerURL + "/mcp"
	r.GitURL = "ssh://git@" + net.JoinHostPort(address, ssh)
	deployment, err := kube.AppsV1().Deployments(cfg.Namespace).Get(ctx, "oberth", metav1.GetOptions{})
	ready := err == nil && deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.ReadyReplicas > 0 && deployment.Spec.Replicas != nil && deployment.Status.ReadyReplicas >= *deployment.Spec.Replicas
	reason := ""
	if !ready {
		reason = deploymentReadinessReason(ctx, cfg, deps)
	}
	r.check("deployment_ready", ready, readinessRemedy(reason))
	r.Checks[len(r.Checks)-1].Reason = reason
	r.Upstream = host.upstream
	if r.Upstream != nil && r.Upstream.State == "pending" {
		r.checkUpstreamPending(*r.Upstream)
		r.check("upstream_public_key", r.Upstream.DeployPublicKey != "", "Read the public deploy key through the selected cluster administration connection.")
	} else {
		upstreamOutput, upstreamErr := deps.RunCommand(ctx, nil, "kubectl", kubectlOberthArgs(cfg, deps, false, "upstream", "list", "--check")...)
		healthy := upstreamErr == nil && verifiedUpstreams(upstreamOutput)
		if r.Upstream != nil && !healthy {
			r.Upstream.State = "pending"
		}
		if !healthy && r.Upstream != nil && upstreamErr == nil && upstreamsVerifiedOrPending(upstreamOutput, true) {
			r.checkUpstreamPending(*r.Upstream)
			r.check("upstream_public_key", r.Upstream.DeployPublicKey != "", "Read the public deploy key through the selected cluster administration connection.")
		} else {
			r.check("upstream", healthy, "Register the upstream deploy public key at the forge, then rerun upstream add and upstream list --check.")
		}

	}

	caPEM, err := serverCACertificate(ctx, cfg, deps)
	ca, parseErr := parseCertificatePEM(caPEM)
	caOK := err == nil && parseErr == nil
	r.check("server_ca", caOK, "Read the server CA through the selected cluster's authenticated admin connection.")
	if caOK {
		sum := sha256.Sum256(ca.Raw)
		r.CAPin = hex.EncodeToString(sum[:])
	}
	if !ready {
		blocked := "blocked by: deployment not ready (" + reason + ")"
		r.check(clientEndpointCheckCode, false, blocked)
		r.Checks[len(r.Checks)-1].Reason = reason
		r.check("ssh_reachable", false, blocked)
		r.Checks[len(r.Checks)-1].Reason = reason
		return r
	}
	if caOK {
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
		connection, dialErr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, https))
		r.check(clientEndpointCheckCode, dialErr == nil, "Make the advertised HTTPS address reachable and ensure its name is on the server certificate.")
		if dialErr != nil {
			failure := clientEndpointFailure(cfg, deps, dialErr)
			r.Checks[len(r.Checks)-1].Reason = failure.check.Reason
			r.Checks[len(r.Checks)-1].Remedy = failure.check.Remedy
		}
		if connection != nil {
			_ = connection.Close()
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, ssh))
	r.check("ssh_reachable", err == nil, "Make the advertised SSH port reachable or set --client-host and --client-ssh-port.")
	if err != nil {
		r.Checks[len(r.Checks)-1].Reason = "SSH endpoint verification at " + r.GitURL + ": " + err.Error()
		r.Checks[len(r.Checks)-1].Remedy = clientEndpointRemedy(cfg, deps)
	}
	if connection != nil {
		_ = connection.Close()
	}
	return r
}

func deploymentReadinessReason(ctx context.Context, cfg Config, deps Deps) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := deps.RunCommand(ctx, nil, "kubectl", kubectlOberthArgs(cfg, deps, false, "admin", "readiness")...)
	var response model.ReadinessResponse
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &response) != nil || !response.Valid() || response.Status == "ready" {
		return "unavailable"
	}
	return response.Code
}

func readinessRemedy(reason string) string {
	switch reason {
	case "no_upstream":
		return "Register an upstream, including pending forge registration, so the server can become ready."
	case "database_error":
		return "Restore database availability; inspect Oberth pod events and storage health."
	case "ssh_identity":
		return "Restore the upstream SSH identity and pinned known-host configuration."
	default:
		return "Inspect Oberth pod events and cached readiness; restore the deployment before checking client connectivity."
	}
}

func PrintVerification(ctx context.Context, cfg Config, deps InstallDeps, asJSON bool) error {
	output := deps.Output
	if asJSON {
		deps.Output = io.Discard
	}
	result := VerifyInstall(ctx, cfg, deps)
	if asJSON {
		if err := json.NewEncoder(output).Encode(result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			_, _ = fmt.Fprintf(output, "%s: ok=%t %s %s\n", check.Code, check.OK, check.Reason, check.Remedy)
			if check.Code == "network_policy" && check.Evidence != "" {
				_, _ = fmt.Fprintln(output, check.Evidence)
			}
		}
	}
	if !result.OK {
		return errors.New("install verification failed; follow the reported remedies")
	}
	return nil
}

func ExecuteJSON(ctx context.Context, cfg Config, deps InstallDeps) error {
	return executeJSON(ctx, cfg, deps, Execute)
}

func executeJSON(ctx context.Context, cfg Config, deps InstallDeps, execute func(context.Context, Config, InstallDeps) error) error {
	output := deps.Output
	deps.Output = io.Discard
	deps.IsTerminal = func() bool { return false }
	deps.OutputIsTerminal = func() bool { return false }
	identity := ""
	var values []string
	deps.secretsSaved = func(keys []string) { values = append([]string(nil), keys...) }
	deps.UplinkCreated = func(value string) { identity = value }
	deps.upstreamResult = func(upstream InstallUpstream) { deps.upstream = &upstream }
	deps.installResolved = func(resolved Config) { cfg = resolved }
	// Keep this invocation's measurement when maintenance verification only
	// reads state. An informational failed probe must not become a pass.
	var measured *NetworkPolicyProbeResult
	probe := deps.ProbeNetworkPolicy
	if probe == nil {
		probe = ProbeNetworkPolicy
	}
	deps.ProbeNetworkPolicy = func(ctx context.Context, cfg Config, raw Deps) NetworkPolicyProbeResult {
		value := probe(ctx, cfg, raw)
		measured = &value
		return value
	}
	installErr := execute(ctx, cfg, deps)
	if cfg.DryRun {
		if installErr != nil {
			review := Review(cfg)
			result := InstallResult{Version: 1, Context: cfg.ContextName, Review: &review, Checks: []CheckResult{}}
			result.check("dry_run", false, "Review the selected context and install inputs, then retry --dry-run --json.")
			var failure *installStepError
			if errors.As(installErr, &failure) {
				result.Checks[0] = failure.check
			}
			if err := json.NewEncoder(output).Encode(result); err != nil {
				return err
			}
			return installErr
		}
		return PrintReviewJSON(output, cfg)
	}
	result := InstallResult{ClientAccess: certificateClientAccess(), Version: 1, OK: installErr == nil, Context: cfg.ContextName, Checks: []CheckResult{}}
	if installErr == nil || errors.Is(installErr, ErrOnboardingPartial) {
		result = verifyInstall(ctx, cfg, deps, cfg.existingRelease)
		if cfg.existingRelease && measured != nil {
			for i := range result.Checks {
				check := &result.Checks[i]
				if check.Code == "network_policy" && check.Warning && check.Reason == "unverified" {
					*check = CheckResult{Code: "network_policy", OK: measured.Status == "enforced", Warning: measured.Status != "enforced", Reason: measured.Status, Remedy: measured.Remedy, Evidence: measured.Evidence}
				}
			}
		}
	}
	var preflight *installPreflightError
	if errors.As(installErr, &preflight) {
		result.Checks = slices.Clone(preflight.checks)
		result.Cluster = &preflight.cluster
		result.Review = &preflight.review
		cfg.ContextName = preflight.review.Context
	}
	if result.Review == nil {
		review := Review(cfg)
		result.Review = &review
	}
	result.Context = cfg.ContextName
	result.Upstream = deps.upstream
	result.FirstUplink = identity
	result.SecretsValues = values
	result.check("install", installErr == nil || errors.Is(installErr, ErrUpstreamKeyPending), "Inspect the selected cluster; finish pending onboarding and rerun --verify --json.")
	var failure *installStepError
	if errors.As(installErr, &failure) {
		result.Checks[len(result.Checks)-1] = failure.check
		result.ServerURL = failure.endpoint
	}
	if cfg.SecretsFile != "" {
		if raw, err := os.ReadFile(cfg.SecretsFile); err == nil { //nolint:gosec // G304: operator-selected credential sink; only its digest is public
			sum := sha256.Sum256(raw)
			clear(raw)
			result.SecretsFile, result.SecretsSHA256 = cfg.SecretsFile, hex.EncodeToString(sum[:])
		}
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	return installResultError(result)
}

// The CLI's tabwriter aligns AUTH with its header. Require a recognized
// successful probe; registration alone does not prove the forge still works.
func verifiedUpstreams(raw []byte) bool { return upstreamsVerifiedOrPending(raw, false) }

func upstreamsVerifiedOrPending(raw []byte, allowPending bool) bool {
	authAt, rows := -1, 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if authAt < 0 {
			if fields[0] == "NAME" {
				authAt = strings.Index(line, "AUTH")
			}
			continue
		}
		if len(fields) < 2 || len(line) <= authAt {
			return false
		}
		status := strings.TrimSpace(line[authAt:])
		healthy := status == "ok" || (strings.HasPrefix(status, "ok (read ") && strings.HasSuffix(status, ")")) || (fields[1] != "ssh" && status == "-")
		pending := allowPending && status == "pending: the forge has not accepted this deploy key"
		if !healthy && !pending {
			return false
		}
		rows++
	}
	return rows > 0
}

func printArgoSecurityReview(w io.Writer, contexts []ArgoSecurityReview) {
	if w == nil {
		return
	}
	for _, security := range contexts {
		before, _ := json.Marshal(security.Current)
		after, _ := json.Marshal(security.Planned)
		_, _ = fmt.Fprintf(w, "Argo %s.%s: %s -> %s (existing values preserved)\n", security.Component, security.Field, before, after)
	}
}

// VerifyInstall reads cluster state and verifies the advertised TLS endpoint
// against the CA obtained over the authenticated Kubernetes connection.
