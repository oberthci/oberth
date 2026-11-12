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
	Context      string                `json:"context"`
	Host         string                `json:"client_host"`
	HTTPS        ClientPortObservation `json:"https"`
	SSH          ClientPortObservation `json:"ssh"`
	InstallFlags []string              `json:"install_flags"`
	NeedsAction  bool                  `json:"needs_action"`
	DNS          *ClientDNSProposal    `json:"dns,omitempty"`
	Advice       string                `json:"advice"`
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
	if net.ParseIP(host) != nil || deps.EndpointAddress != "" {
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
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.InstallFlags = []string{"--client-host", result.Host, "--client-https-port", result.HTTPS.Port, "--client-ssh-port", result.SSH.Port}
	result.Advice = "TCP reachability and published mappings do not prove Oberth readiness or TLS identity. Review these client ports before install; verify the server afterwards."
	if result.NeedsAction {
		result.Advice = "Planned client ports did not answer from this host and no unique local mapping was found. NodePorts are not reachable from this host; map them or use port-forward, then repeat this probe. A fresh server may not be listening yet: review the network path, not the cluster type."
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
	clusters := map[string]bool{}
	for _, node := range plan.Identity.Nodes {
		if !probeContainerName.MatchString(node.Name) {
			continue
		}
		raw, err := deps.Run(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "k3d.cluster"}}`, node.Name)
		cluster := strings.TrimSpace(string(raw))
		if err == nil && probeContainerName.MatchString(cluster) {
			clusters[cluster] = true
		}
	}
	candidates := map[string]map[string]bool{}
	for cluster := range clusters {
		raw, err := deps.Run(ctx, "docker", "ps", "--filter", "label=k3d.cluster="+cluster, "--format", "{{.ID}}")
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
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return true
	}
	ip := net.ParseIP(bind)
	if host == "localhost" {
		return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
	}
	return ip != nil && ip.Equal(net.ParseIP(host))
}
