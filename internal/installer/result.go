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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// CheckResult is a stable public check. Diagnostics never include subprocess output.
type CheckResult struct {
	Code   string `json:"code"`
	OK     bool   `json:"ok"`
	Remedy string `json:"remedy,omitempty"`
}

type InstallReview struct {
	ReissueServerCA bool     `json:"reissue_server_ca"`
	Context         string   `json:"context"`
	Namespace       string   `json:"namespace"`
	ArgoNamespace   string   `json:"argo_namespace"`
	ProductionStore bool     `json:"production_store"`
	DevStore        bool     `json:"dev_store"`
	ClientHost      string   `json:"client_host"`
	AdminIdentity   string   `json:"admin_identity"`
	DNSNames        []string `json:"tls_dns_names"`
	IPs             []string `json:"tls_ips"`
}

func Review(cfg Config) InstallReview {
	return InstallReview{ReissueServerCA: cfg.ReissueServerCA, Context: cfg.ContextName, Namespace: cfg.Namespace, ArgoNamespace: cfg.ArgoNamespace, ProductionStore: cfg.InstallSecretStore, DevStore: cfg.InstallSecretStoreDev, ClientHost: cfg.ClientHost, AdminIdentity: cfg.UplinkIdentity, DNSNames: cfg.TLSExtraDNSNames, IPs: cfg.TLSExtraIPs}
}

func PrintReviewJSON(w io.Writer, cfg Config) error {
	return json.NewEncoder(w).Encode(struct {
		Version              int           `json:"version"`
		Review               InstallReview `json:"review"`
		ConfirmationRequired bool          `json:"confirmation_required"`
	}{1, Review(cfg), true})
}

type InstallResult struct {
	Cluster       *ClusterIdentity `json:"cluster,omitempty"`
	SecretsValues []string         `json:"secrets_values,omitempty"`
	Review        *InstallReview   `json:"review,omitempty"`
	Version       int              `json:"version"`
	OK            bool             `json:"ok"`
	Context       string           `json:"context"`
	ServerURL     string           `json:"server_url,omitempty"`
	MCPURL        string           `json:"mcp_url,omitempty"`
	GitURL        string           `json:"git_url,omitempty"`
	CAPin         string           `json:"ca_sha256,omitempty"`
	FirstUplink   string           `json:"first_uplink,omitempty"`
	SecretsFile   string           `json:"secrets_file,omitempty"`
	SecretsSHA256 string           `json:"secrets_sha256,omitempty"`
	Checks        []CheckResult    `json:"checks"`
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
func PrintReview(w io.Writer, cfg Config) {
	_, _ = fmt.Fprintf(w, "Review install: context %q, namespace %q, pipeline namespace %q\nSecret store: production=%t dev=%t; client address=%q; admin identity=%q\nTLS DNS names: %v; IP addresses: %v\n", cfg.ContextName, cfg.Namespace, cfg.ArgoNamespace, cfg.InstallSecretStore, cfg.InstallSecretStoreDev, cfg.ClientHost, cfg.UplinkIdentity, cfg.TLSExtraDNSNames, cfg.TLSExtraIPs)
}

// VerifyInstall reads cluster state and verifies the advertised TLS endpoint
// against the CA obtained over the authenticated Kubernetes connection.
func VerifyInstall(ctx context.Context, cfg Config, host InstallDeps) InstallResult {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	host.Output = io.Discard
	host = host.withDefaults()
	r := InstallResult{Version: 1, OK: true, Context: cfg.ContextName, Checks: []CheckResult{}}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.KubeconfigPath != "" {
		host.LoadKubeConfig = func(name string) (kubernetes.Interface, *rest.Config, string, error) {
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
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
	deps := Deps{KubeClient: kube, RestConfig: config, ContextName: selected, ClientHost: cfg.ClientHost, Output: io.Discard, RunCommand: run}
	identity := publicClusterIdentity(cfg, config, selected)
	r.check("cluster_ca", identity.CAPin != "" && !config.Insecure, "Use a kubeconfig with an explicit trusted cluster CA and TLS verification enabled.")
	preflightDeps := deps
	preflightDeps.RunCommand = host.RunCommand
	inspectClusterPreflight(ctx, &cfg, preflightDeps, &identity, &r)
	r.Cluster = &identity
	service, serviceErr := kube.CoreV1().Services(cfg.Namespace).Get(ctx, "oberth", metav1.GetOptions{})
	deps.ClientServiceType = cfg.ServiceType
	deps = WithServiceEndpoint(service, cfg.Namespace, deps)
	r.check("service", serviceErr == nil, "Restore the Oberth Service and Kubernetes read permissions.")
	deps = withClientPorts(cfg, deps)
	ssh, https := clientPorts(deps)
	address := clientReachableHost(deps)
	r.ServerURL = "https://" + net.JoinHostPort(address, https)
	r.MCPURL = r.ServerURL + "/mcp"
	r.GitURL = "ssh://git@" + net.JoinHostPort(address, ssh)
	deployment, err := kube.AppsV1().Deployments(cfg.Namespace).Get(ctx, "oberth", metav1.GetOptions{})
	ready := err == nil && deployment.Status.ReadyReplicas > 0 && deployment.Spec.Replicas != nil && deployment.Status.ReadyReplicas >= *deployment.Spec.Replicas
	r.check("deployment_ready", ready, "Check Oberth pod events, unseal OpenBao, and finish upstream registration.")
	upstreamOutput, upstreamErr := deps.RunCommand(ctx, nil, "kubectl", kubectlOberthArgs(cfg, deps, false, "upstream", "list", "--check")...)
	r.check("upstream", upstreamErr == nil && verifiedUpstreams(upstreamOutput), "Register the upstream deploy public key at the forge, then rerun upstream add and upstream list --check.")
	caPEM, err := serverCACertificate(ctx, cfg, deps)
	ca, parseErr := parseCertificatePEM(caPEM)
	caOK := err == nil && parseErr == nil
	r.check("server_ca", caOK, "Read the server CA through the selected cluster's authenticated admin connection.")
	if caOK {
		sum := sha256.Sum256(ca.Raw)
		r.CAPin = hex.EncodeToString(sum[:])
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
		connection, dialErr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, https))
		r.check("https_tls", dialErr == nil, "Make the advertised HTTPS address reachable and ensure its name is on the server certificate.")
		if connection != nil {
			_ = connection.Close()
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, ssh))
	r.check("ssh_reachable", err == nil, "Make the advertised SSH port reachable or set --client-host and --client-ssh-port.")
	if connection != nil {
		_ = connection.Close()
	}
	return r
}

func PrintVerification(ctx context.Context, cfg Config, deps InstallDeps, asJSON bool) error {
	result := VerifyInstall(ctx, cfg, deps)
	if asJSON {
		if err := json.NewEncoder(deps.Output).Encode(result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			_, _ = fmt.Fprintf(deps.Output, "%s: ok=%t %s\n", check.Code, check.OK, check.Remedy)
		}
	}
	if !result.OK {
		return errors.New("install verification failed; follow the reported remedies")
	}
	return nil
}

func ExecuteJSON(ctx context.Context, cfg Config, deps InstallDeps) error {
	if cfg.DryRun {
		return errors.New("use --dry-run without --json to review the install")
	}
	output := deps.Output
	deps.Output = io.Discard
	deps.IsTerminal = func() bool { return false }
	deps.OutputIsTerminal = func() bool { return false }
	identity := ""
	var values []string
	deps.secretsSaved = func(keys []string) { values = append([]string(nil), keys...) }
	deps.UplinkCreated = func(value string) { identity = value }
	installErr := Execute(ctx, cfg, deps)
	result := InstallResult{Version: 1, OK: installErr == nil, Context: cfg.ContextName, Checks: []CheckResult{}}
	if installErr == nil || errors.Is(installErr, ErrOnboardingPartial) {
		result = VerifyInstall(ctx, cfg, deps)
	}
	if errors.Is(installErr, errInstallPreflight) {
		plan := PlanInstall(ctx, cfg, deps)
		result.Checks = plan.Checks
		result.Cluster = &plan.Identity
	}
	review := Review(cfg)
	result.Review = &review
	result.FirstUplink = identity
	result.SecretsValues = values
	result.check("install", installErr == nil, "Inspect the selected cluster; finish pending onboarding and rerun --verify --json.")
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
	if !result.OK {
		return errors.New("install did not pass all checks; follow the reported remedies")
	}
	return nil
}

// The CLI's tabwriter aligns AUTH with its header. Require a recognized
// successful probe; registration alone does not prove the forge still works.
func verifiedUpstreams(raw []byte) bool {
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
		if !healthy {
			return false
		}
		rows++
	}
	return rows > 0
}
