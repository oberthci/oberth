package installer

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const clientEndpointCheckCode = "client_bootstrap_endpoint"

// Only diagnostics produced locally from public configuration belong here.
// Subprocess errors/output can contain credentials and must not be serialized.
type installStepError struct {
	check    CheckResult
	endpoint string
}

func (e *installStepError) Error() string {
	return fmt.Sprintf("%s: %s; %s", e.check.Code, e.check.Reason, e.check.Remedy)
}

func clientRoutingRemedy(cfg Config, deps Deps) string {
	ssh, https := clientPorts(deps)
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	command := printedKubectl(cfg, []string{"--context", deps.ContextName, "-n", cfg.Namespace, "port-forward", "service/oberth", https + ":https", ssh + ":ssh"})
	return "The client endpoint must be reachable from this machine. Start the documented port-forward in another terminal (" + command + ") for a loopback client host, or choose a reachable --client-host with the required DNS/hosts entry and matching TLS names."
}

func clientEndpointRemedy(cfg Config, deps Deps) string {
	state := "Oberth and Argo remain deployed"
	if cfg.wantsSecretStore() {
		state += ", along with OpenBao"
	}
	if cfg.InstallRekor {
		state += " and Rekor"
	}
	return clientRoutingRemedy(cfg, deps) + " " + state + ". Saved credentials and any issued client certificate stay on disk. Client access is not yet verified. Resume with the documented verification command after fixing the route: " + clientVerificationCommand(cfg, deps) + ". Verification continues from the deployed state without reissuing the client certificate. If checks show unfinished onboarding, rerun the approved install using --resume-secrets-file for saved credentials and a new --secrets-file for new credentials."
}

func clientEndpointFailure(cfg Config, deps Deps, err error) *installStepError {
	_, https := clientPorts(deps)
	endpoint := "https://" + net.JoinHostPort(clientReachableHost(deps), https)
	return &installStepError{check: CheckResult{Code: clientEndpointCheckCode, Reason: "client bootstrap endpoint verification at " + endpoint + ": " + err.Error(), Remedy: clientEndpointRemedy(cfg, deps)}, endpoint: endpoint}
}

func requireClientEndpoint(ctx context.Context, cfg Config, deps Deps) error {
	_, https := clientPorts(deps)
	address := net.JoinHostPort(clientReachableHost(deps), https)
	// A Running Pod can still be starting its listener. Bound that wait; never
	// start a forward, edit hosts or change the chosen advertised endpoint.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return clientEndpointFailure(cfg, deps, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func clientRequirements(cfg Config) []string {
	host := cfg.ClientHost
	if host == "" {
		if cfg.ServiceType == "ClusterIP" {
			return []string{"Required before client bootstrap: a ClusterIP needs an internal route from this machine, or an operator-started port-forward with an explicit loopback --client-host and client port flags. Arrange that route before onboarding; the installer does not start a forward."}
		}
		return nil
	}
	deps := withClientEndpoint(cfg, Deps{ClientServiceType: cfg.ServiceType, ContextName: cfg.ContextName})
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) || cfg.ServiceType == "ClusterIP" {
		return []string{"Required before client bootstrap: the chosen client host " + host + " must route to Oberth from this machine. A remote cluster does not publish ports on this machine's loopback. Arrange port mapping or start the documented port-forward yourself after deployment; the installer stops at client bootstrap until the endpoint answers. " + clientRoutingRemedy(cfg, deps)}
	}
	if ip == nil {
		return []string{"Required before client bootstrap: " + host + " must resolve to a reachable Oberth address from this machine. Create any required DNS/hosts entry yourself before continuing; the installer does not edit hosts or start a port-forward."}
	}
	return nil
}

// A new Pod deliberately stays out of Service endpoints until registration.
// Check client reachability only after that readiness transition, before any
// announcement that the advertised client endpoint is ready.
func waitForClientReady(ctx context.Context, cfg Config, deps Deps) error {
	if err := WaitForReady(ctx, cfg, deps); err != nil {
		return err
	}
	if deps.checkClientReachability {
		return requireClientEndpoint(ctx, cfg, deps)
	}
	return nil
}

func clientVerificationCommand(cfg Config, deps Deps) string {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	ssh, https := clientPorts(deps)
	args := []string{"oberth", "install", "--verify", "--json", "--context", deps.ContextName, "--namespace", ns, "--client-host", clientReachableHost(deps), "--client-https-port", https, "--client-ssh-port", ssh}
	if cfg.KubeconfigPath != "" {
		args = append(args, "--kubeconfig", cfg.KubeconfigPath)
	}
	for i := range args {
		args[i] = shellQuoteWord(args[i])
	}
	return strings.Join(args, " ")
}
