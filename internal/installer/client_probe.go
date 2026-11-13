package installer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ClientPortProbe is advisory: a TCP listener is not Oberth readiness or TLS trust.
type ClientPortProbe struct {
	Context      string                      `json:"context"`
	Host         string                      `json:"client_host"`
	HTTPS        ClientPortObservation       `json:"https"`
	SSH          ClientPortObservation       `json:"ssh"`
	InstallFlags []string                    `json:"install_flags"`
	NeedsAction  bool                        `json:"needs_action"`
	KindRemedy   *ClientKindRemedy           `json:"kind_remedy,omitempty"`
	LoadBalancer *ClientLoadBalancerProposal `json:"load_balancer,omitempty"`
	DNS          *ClientDNSProposal          `json:"dns,omitempty"`
	Advice       string                      `json:"advice"`
}

type ClientPortObservation struct {
	Port      string `json:"port"`
	Reachable bool   `json:"tcp_reachable"`
	Published bool   `json:"published_mapping"`
}

type ClientPortProbeDeps struct {
	Run             func(context.Context, string, ...string) ([]byte, error)
	Dial            func(context.Context, string) error
	LookupHost      func(context.Context, string) ([]string, error)
	EndpointAddress string
	DockerHost      string
	DockerContext   string
}

func DefaultClientPortProbeDeps() ClientPortProbeDeps {
	return ClientPortProbeDeps{LookupHost: net.DefaultResolver.LookupHost, DockerHost: os.Getenv("DOCKER_HOST"), DockerContext: os.Getenv("DOCKER_CONTEXT"), Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		commandCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		// Only selected public labels and port bindings are requested. Never inspect
		// full container configuration, which can contain credentials.
		return exec.CommandContext(commandCtx, name, args...).Output() //nolint:gosec // G204: callers below use only Docker read-only inspection with separate arguments.
	}, Dial: func(ctx context.Context, address string) error {
		dialer := net.Dialer{Timeout: time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		return conn.Close()
	}}
}

func ProbeClientPorts(ctx context.Context, plan InstallPlan, cfg Config, deps ClientPortProbeDeps) (ClientPortProbe, error) {
	result := ClientPortProbe{Context: plan.Identity.Context, Host: cfg.ClientHost}
	if plan.Version != 1 || plan.Identity.Context == "" || len(plan.Identity.Nodes) == 0 {
		return result, errors.New("client ports requires a cluster plan with the selected context and nodes")
	}
	if result.Host == "" {
		result.Host = plan.Review.ClientHost
	}
	if result.Host == "" {
		return result, errors.New("choose the advertised address with --client-host before probing")
	}
	host, err := NormalizeClientHost(result.Host)
	if err != nil {
		return result, errors.New("invalid advertised client host")
	}
	result.Host = host
	if deps.EndpointAddress != "" {
		if _, err := NormalizeClientHost(deps.EndpointAddress); err != nil {
			return result, errors.New("invalid endpoint address")
		}
	}
	if net.ParseIP(host) == nil && deps.EndpointAddress != "" {
		proposal, err := proposeClientDNS(ctx, host, deps.EndpointAddress, deps.LookupHost)
		if err != nil {
			return result, err
		}
		result.DNS = &proposal
		result.Host = proposal.Host
		result.NeedsAction = !proposal.Resolves
	}
	switch cfg.ServiceType {
	case "", "NodePort", "ClusterIP", "LoadBalancer":
	default:
		return result, errors.New("invalid service type")
	}
	ssh, https := clientPorts(Deps{ClientServiceType: cfg.ServiceType, ClientSSHPort: cfg.ClientSSHPort, ClientHTTPSPort: cfg.ClientHTTPSPort})
	for _, port := range []string{ssh, https} {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return result, errors.New("client ports must be decimal numbers from 1 to 65535")
		}
	}
	result.HTTPS.Port, result.SSH.Port = https, ssh
	defaultSSH, defaultHTTPS := clientPorts(Deps{ClientServiceType: cfg.ServiceType})
	portsNeedAction := false
	mappings := localPublishedPorts(ctx, plan, result.Host, deps)
	for _, item := range []struct {
		observation      *ClientPortObservation
		explicit, target string
	}{{&result.HTTPS, cfg.ClientHTTPSPort, defaultHTTPS}, {&result.SSH, cfg.ClientSSHPort, defaultSSH}} {
		if item.explicit == "" && len(mappings[item.target]) == 1 {
			item.observation.Port = mappings[item.target][0]
			item.observation.Published = true
		}
		if item.explicit != "" {
			for _, port := range mappings[item.target] {
				if port == item.explicit {
					item.observation.Published = true
				}
			}
		}
		if deps.Dial != nil {
			item.observation.Reachable = deps.Dial(ctx, net.JoinHostPort(result.Host, item.observation.Port)) == nil
		}
		if !item.observation.Reachable && !item.observation.Published {
			result.NeedsAction = true
			portsNeedAction = true
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.InstallFlags = []string{"--client-host", result.Host, "--client-https-port", result.HTTPS.Port, "--client-ssh-port", result.SSH.Port}
	if cfg.ServiceType != "" && cfg.ServiceType != "NodePort" {
		result.InstallFlags = append(result.InstallFlags, "--service-type", cfg.ServiceType)
	}
	result.Advice = "TCP reachability and published mappings do not prove Oberth readiness or TLS identity. Review these client ports before install; verify the server afterwards."
	if portsNeedAction {
		var missing []string
		for _, item := range []struct {
			name        string
			observation ClientPortObservation
		}{{"HTTPS", result.HTTPS}, {"SSH", result.SSH}} {
			if !item.observation.Reachable && !item.observation.Published {
				missing = append(missing, item.name+" port "+item.observation.Port)
			}
		}
		observation := "TCP reachability is unverified for "
		if deps.Dial != nil {
			observation = "TCP probes did not answer for "
		}
		result.Advice += " " + observation + strings.Join(missing, ", ") + "; no unique local mapping was found for those ports. This does not establish a missing route. A fresh server may not be listening yet; repeat the probe after install. Reachability by node IP depends on the network between client and node: NAT-isolated guests cannot reach each other. If access still fails, review the network path and use a name or IP routable from the client, or port-forward."
	}
	if plan.Identity.Distribution == "kind" && (cfg.ServiceType == "" || cfg.ServiceType == "NodePort") && (!result.HTTPS.Published || !result.SSH.Published) {
		result.KindRemedy = clientKindRemedy(result.HTTPS.Port, result.SSH.Port)
	}
	if portsNeedAction && deps.Dial != nil && (cfg.ServiceType == "" || cfg.ServiceType == "NodePort") {
		nodeReachable := reachableProbeNode(ctx, plan, deps)
		if !nodeReachable {
			result.LoadBalancer = &ClientLoadBalancerProposal{InstallFlags: []string{"--service-type", "LoadBalancer", "--client-https-port", "443", "--client-ssh-port", "2222"}, Advice: "No node address answered the planned NodePorts from this client. Offer LoadBalancer in the same install Review, or map ports/use port-forward. After the approved install starts, wait for its Service address with --wait-load-balancer, then choose its routable IP or DNS name and re-probe. The server certificate must cover that choice; an IP needs a matching IP SAN."}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.DNS != nil && !result.DNS.Resolves {
		result.Advice += " Show the DNS remedy, wait for the user to configure it, then repeat this probe with the proposed DNS name and the same --endpoint-address before continuing."
	}
	return result, nil
}

var probeContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,252}$`)

// Discover only containers belonging to nodes in the selected plan, then their
// cluster's published ports (including serverlb and direct node mappings).
// A remote Docker daemon's host bindings never describe this client's localhost.
func localPublishedPorts(ctx context.Context, plan InstallPlan, host string, deps ClientPortProbeDeps) map[string][]string {
	result := map[string][]string{}
	if deps.Run == nil || !isLoopbackHost(host) {
		return result
	}
	daemon := deps.DockerHost
	if daemon == "" || deps.DockerContext != "" {
		args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
		if deps.DockerContext != "" {
			if !probeContainerName.MatchString(deps.DockerContext) {
				return result
			}
			args = append(args, deps.DockerContext)
		}
		raw, err := deps.Run(ctx, "docker", args...)
		if err != nil {
			return result
		}
		daemon = strings.TrimSpace(string(raw))
	}
	if !strings.HasPrefix(daemon, "unix://") && !strings.HasPrefix(daemon, "npipe://") {
		return result
	}
	clusters := map[string]string{}
	for _, node := range plan.Identity.Nodes {
		if !probeContainerName.MatchString(node.Name) {
			continue
		}
		raw, err := deps.Run(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "k3d.cluster"}}`, node.Name)
		cluster := strings.TrimSpace(string(raw))
		if err == nil && probeContainerName.MatchString(cluster) {
			clusters[cluster] = "k3d.cluster="
		} else {
			raw, err = deps.Run(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "io.x-k8s.kind.cluster"}}`, node.Name)
			cluster = strings.TrimSpace(string(raw))
			if err == nil && probeContainerName.MatchString(cluster) {
				clusters[cluster] = "io.x-k8s.kind.cluster="
			}
		}
	}
	candidates := map[string]map[string]bool{}
	for cluster, label := range clusters {
		raw, err := deps.Run(ctx, "docker", "ps", "--filter", "label="+label+cluster, "--format", "{{.ID}}")
		if err != nil {
			continue
		}
		for _, id := range strings.Fields(string(raw)) {
			if !probeContainerName.MatchString(id) {
				continue
			}
			raw, err = deps.Run(ctx, "docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", id)
			var ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			}
			if err != nil || json.Unmarshal(raw, &ports) != nil {
				continue
			}
			for target, bindings := range ports {
				if !strings.HasSuffix(target, "/tcp") {
					continue
				}
				target = strings.TrimSuffix(target, "/tcp")
				for _, binding := range bindings {
					n, err := strconv.Atoi(binding.HostPort)
					if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != binding.HostPort || !probeBindingMatches(host, binding.HostIP) {
						continue
					}
					if candidates[target] == nil {
						candidates[target] = map[string]bool{}
					}
					candidates[target][binding.HostPort] = true
				}
			}
		}
	}
	for target, ports := range candidates {
		for port := range ports {
			result[target] = append(result[target], port)
		}
		sort.Strings(result[target])
	}
	return result
}

func probeBindingMatches(host, bind string) bool {
	ip := net.ParseIP(bind)
	if host == "localhost" {
		return bind == "" || (ip != nil && (ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)))
	}
	clientIP := net.ParseIP(host)
	if ip == nil || clientIP == nil {
		return false
	}
	if ip.IsUnspecified() {
		// A wildcard binding proves publication only for its own address family.
		// A dual-stack listener may accept more; a successful TCP probe can show it.
		return (ip.To4() == nil) == (clientIP.To4() == nil)
	}
	return ip.Equal(clientIP)
}

// Bound advisory node checks independently so an unreachable large cluster
// still receives a network proposal within the overall probe budget.
func reachableProbeNode(ctx context.Context, plan InstallPlan, deps ClientPortProbeDeps) bool {
	if deps.Dial == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, node := range plan.Identity.Nodes {
		for _, address := range node.Addresses {
			if ctx.Err() != nil {
				return false
			}
			if net.ParseIP(address) == nil {
				continue
			}
			if deps.Dial(ctx, net.JoinHostPort(address, httpsNodePort)) == nil || deps.Dial(ctx, net.JoinHostPort(address, sshNodePort)) == nil {
				return true
			}
		}
	}
	return false
}
