package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const k3sCopyCommand = `mkdir -p "$HOME/.kube" && chmod 700 "$HOME/.kube" && sudo install -m 600 -o "$(id -u)" -g "$(id -g)" /etc/rancher/k3s/k3s.yaml "$HOME/.kube/config"`

var errInstallPreflight = errors.New("install preflight failed")

// Keep the apply observation intact when reporting a failed install as JSON.
// A read-only plan cannot reconstruct live measurement or cleanup evidence.
type installPreflightError struct {
	checks  []CheckResult
	cluster ClusterIdentity
	review  InstallReview
}

func (e *installPreflightError) Error() string {
	var failures []string
	for _, check := range e.checks {
		if !check.OK && !check.Warning {
			failures = append(failures, check.Code+": "+check.Remedy)
		}
	}
	return errInstallPreflight.Error() + ":\n" + strings.Join(failures, "\n")
}

func (e *installPreflightError) Unwrap() error { return errInstallPreflight }

var errFreshK3s = errors.New("fresh k3s has no user kubeconfig")

// Only suggest the operator's copy command when no user configuration exists.
// Never select the root kubeconfig or change its permissions automatically.
func freshK3sHint(rules *clientcmd.ClientConfigLoadingRules, path string) error {
	if path == "" || rules.ExplicitPath != "" || os.Getenv("KUBECONFIG") != "" {
		return nil
	}
	for _, candidate := range rules.Precedence {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if _, err := os.Stat(path); err == nil || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w; run this command yourself, then retry (the server address remains 127.0.0.1):\n%s", errFreshK3s, k3sCopyCommand)
	}
	return nil
}

type NodeIdentity struct {
	Addresses         []string `json:"addresses,omitempty"`
	Name              string   `json:"name"`
	Architecture      string   `json:"architecture"`
	KubernetesVersion string   `json:"kubernetes_version"`
}

type ClusterIdentity struct {
	Files        []string       `json:"files"`
	Context      string         `json:"context"`
	Server       string         `json:"api_server"`
	CAPin        string         `json:"ca_sha256,omitempty"`
	Distribution string         `json:"distribution"`
	Nodes        []NodeIdentity `json:"nodes"`
}

type InstallPlan struct {
	Version  int             `json:"version"`
	OK       bool            `json:"ok"`
	Identity ClusterIdentity `json:"identity"`
	Review   InstallReview   `json:"review"`
	Checks   []CheckResult   `json:"checks"`
}

func selectedRules(cfg Config) *clientcmd.ClientConfigLoadingRules {
	rules := kubeconfig.Rules("")
	rules.ExplicitPath = cfg.KubeconfigPath
	return rules
}

func publicClusterIdentity(cfg Config, config *rest.Config, selected string) ClusterIdentity {
	identity := ClusterIdentity{Context: selected, Files: []string{}, Nodes: []NodeIdentity{}}
	rules := selectedRules(cfg)
	candidates := rules.Precedence
	if rules.ExplicitPath != "" {
		candidates = []string{rules.ExplicitPath}
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			identity.Files = append(identity.Files, path)
		}
	}
	if config == nil {
		return identity
	}
	if parsed, err := url.Parse(config.Host); err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		identity.Server = parsed.String()
	}
	ca := config.CAData
	if len(ca) == 0 && config.CAFile != "" {
		ca, _ = os.ReadFile(config.CAFile)
	} //nolint:gosec // G304: public CA from the selected kubeconfig
	if certificate, err := parseCertificatePEM(ca); err == nil {
		sum := sha256.Sum256(certificate.Raw)
		identity.CAPin = hex.EncodeToString(sum[:])
	}
	return identity
}

func PlanInstall(ctx context.Context, cfg Config, host InstallDeps) InstallPlan {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	host.Output = io.Discard
	host = host.withDefaults()
	r := InstallResult{OK: true, Checks: []CheckResult{}}
	plan := InstallPlan{Version: 1, Review: Review(cfg), Identity: ClusterIdentity{Context: cfg.ContextName, Files: []string{}, Nodes: []NodeIdentity{}}}
	rules := selectedRules(cfg)
	plan.Identity = publicClusterIdentity(cfg, nil, cfg.ContextName)
	for _, tool := range []string{"helm", "kubectl"} {
		_, err := host.LookPath(tool)
		r.check("tool_"+tool, err == nil, "Install "+tool+" yourself and put it on PATH; the installer never installs prerequisites.")
	}
	if host.KubeContexts != nil {
		err := RequireContextChoice("install on", cfg.ContextName, cfg.KubeconfigPath, host.KubeContexts)
		r.check("context_choice", err == nil, "Select the intended context with --context; every cluster uses the same Review confirmation.")
		if err != nil {
			plan.OK, plan.Checks = r.OK, r.Checks
			return plan
		}
	}
	var kube kubernetes.Interface
	var config *rest.Config
	var selected string
	var err error
	if cfg.KubeconfigPath != "" {
		kube, config, selected, err = loadKubeConfigWithRules(rules, cfg.ContextName, "", io.Discard)
	} else {
		kube, config, selected, err = host.LoadKubeConfig(cfg.ContextName)
	}
	remedy := "Select a readable kubeconfig and context with valid credentials. KUBECONFIG may list files separated by the platform path separator."
	if errors.Is(err, errFreshK3s) {
		remedy = k3sCopyCommand
	}
	r.check("kubeconfig", err == nil, remedy)
	if err != nil {
		plan.OK, plan.Checks = r.OK, r.Checks
		return plan
	}
	plan.Identity = publicClusterIdentity(cfg, config, selected)
	plan.Review.Context = selected
	if !checkClusterTrust(config, plan.Identity, &r) {
		plan.OK, plan.Checks = r.OK, r.Checks
		return plan
	}
	run := host.RunCommand
	deps := Deps{KubeClient: kube, RestConfig: config, ContextName: selected, RunCommand: run}
	inspectInstallPreflight(ctx, &cfg, deps, host, selected, &plan.Identity, &r, false)
	plan.Review = Review(cfg)
	plan.Review.Context = selected
	plan.OK, plan.Checks = r.OK, r.Checks
	return plan
}

// Discover stored configuration before checking capability. Planning only reads;
// apply measures enforcement before any release or namespace mutation.
func inspectInstallPreflight(ctx context.Context, cfg *Config, deps Deps, host InstallDeps, selected string, identity *ClusterIdentity, r *InstallResult, measure bool) {
	if err := readInstallValues(ctx, cfg, installPolicyDeps(host, *cfg, selected)); err != nil {
		r.check("installed_values", false, "Read the existing Helm release values before choosing policy configuration.")
		return
	}
	if err := readArgoReviewValues(ctx, cfg, installPolicyDeps(host, *cfg, selected)); err != nil {
		r.check("argo_values", false, argoValuesRemedy)
		return
	}
	if err := resolveArgoExecutorResources(ctx, cfg, installPolicyDeps(host, *cfg, selected)); err != nil {
		r.check("argo_executor_resources", false, "Read the stored argo-workflows values so operator executor.resources can be preserved.")
		return
	}
	if cfg.existingRelease {
		cfg.NetworkPolicy = "auto"
	}

	securityDeps := installPolicyDeps(host, *cfg, selected)
	securityDeps.KubeClient = deps.KubeClient
	if measure {
		securityDeps.Input, securityDeps.IsTerminal = host.Input, host.IsTerminal
	}
	if err := readArgoSecurityReview(ctx, cfg, securityDeps); err != nil {
		r.check("argo_security_values", false, err.Error())
		return
	}

	inspectCluster(ctx, cfg, deps, identity, r, cfg.existingRelease, measure)
}

func inspectCluster(ctx context.Context, cfg *Config, deps Deps, identity *ClusterIdentity, r *InstallResult, maintenance, measure bool) {
	nodes, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	r.check("nodes", err == nil && len(nodes.Items) > 0, "Grant read access to nodes and bring at least one node Ready.")
	if err != nil {
		return
	}
	cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = SingleHostTLSNames(nodes.Items, cfg.TLSExtraDNSNames, cfg.TLSExtraIPs)
	identity.Distribution = DetectEngine(nodes.Items)
	if identity.Distribution == "unknown" {
		for _, node := range nodes.Items {
			if node.Spec.ProviderID != "" {
				identity.Distribution = "managed"
				break
			}
		}
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	for _, node := range nodes.Items {
		item := NodeIdentity{Name: node.Name, Architecture: node.Status.NodeInfo.Architecture, KubernetesVersion: node.Status.NodeInfo.KubeletVersion}
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeExternalIP || address.Type == corev1.NodeInternalIP {
				item.Addresses = append(item.Addresses, address.Address)
			}
		}
		r.check("architecture/"+node.Name, item.Architecture == "amd64" || item.Architecture == "arm64", "Use an amd64 or arm64 node supported by the published Oberth images.")
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
		}
		r.check("node_ready/"+node.Name, ready, "Restore node readiness before installing.")
		identity.Nodes = append(identity.Nodes, item)
	}
	classes, err := deps.KubeClient.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	defaultClass := false
	if err == nil {
		for _, class := range classes.Items {
			if class.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				defaultClass = true
			}
		}
	}
	r.check("default_storage_class", defaultClass, "Configure a default StorageClass with a working provisioner for persistent OpenBao and Oberth data.")
	if cfg.NetworkPolicy == "false" && !maintenance {
		r.check("network_policy", false, "Pipeline NetworkPolicy is disabled; enable it and repeat the live enforcement probe.")
		r.Checks[len(r.Checks)-1].Reason = "disabled"
		return
	}
	if !measure {
		r.Checks = append(r.Checks, CheckResult{Code: "network_policy", Warning: true, Reason: "unverified", Remedy: "NetworkPolicy enforcement check will run first at install, after approval. It creates temporary probe Pods and their scratch namespace and cleans them up; fresh installs refuse unless enforcement is measured."})
		return
	}
	if !r.OK {
		r.check("network_policy", false, "NetworkPolicy enforcement is unverified; resolve the earlier cluster checks and repeat the live probe.")
		r.Checks[len(r.Checks)-1].Reason = "unverified"
		return
	}
	enforcement := measureNetworkPolicy(ctx, *cfg, deps)
	if maintenance {
		r.Checks = append(r.Checks, CheckResult{Code: "network_policy", OK: enforcement.Status == "enforced", Warning: enforcement.Status != "enforced", Reason: enforcement.Status, Remedy: enforcement.Remedy, Evidence: enforcement.Evidence})
		return
	}
	r.check("network_policy", enforcement.Status == "enforced", enforcement.Remedy)
	r.Checks[len(r.Checks)-1].Reason = enforcement.Status
	r.Checks[len(r.Checks)-1].Evidence = enforcement.Evidence
}

func PrintPlan(ctx context.Context, cfg Config, deps InstallDeps, asJSON bool) error {
	plan := PlanInstall(ctx, cfg, deps)
	if asJSON {
		if err := json.NewEncoder(deps.Output).Encode(plan); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(deps.Output, "Cluster: %s %s; CA sha256 %s; files %v\n", plan.Identity.Context, plan.Identity.Server, plan.Identity.CAPin, plan.Identity.Files)
		printReview(deps.Output, plan.Review)
		for _, check := range plan.Checks {
			_, _ = fmt.Fprintf(deps.Output, "%s: ok=%t %s %s\n", check.Code, check.OK, check.Reason, check.Remedy)
			if check.Code == "network_policy" && check.Evidence != "" {
				_, _ = fmt.Fprintln(deps.Output, check.Evidence)
			}
		}
	}
	if !plan.OK {
		return errors.New("install preflight failed; follow the reported remedies")
	}
	return nil
}

// A rejected trust check must stop before credentials are sent to the API.
func checkClusterTrust(config *rest.Config, identity ClusterIdentity, result *InstallResult) bool {
	trusted := config != nil && !config.Insecure && identity.CAPin != "" && strings.HasPrefix(identity.Server, "https://")
	result.check("cluster_ca", trusted, "Use an HTTPS kubeconfig with an explicit trusted cluster CA and TLS verification enabled.")
	return trusted
}
