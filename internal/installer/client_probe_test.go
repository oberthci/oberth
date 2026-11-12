package installer

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
)

func probePlan() InstallPlan {
	return InstallPlan{Version: 1, Identity: ClusterIdentity{Context: "selected", Distribution: "unknown", Nodes: []NodeIdentity{{Name: "k3d-example-server-0"}}}, Review: InstallReview{ClientHost: "localhost"}}
}

func probeDocker(t *testing.T, ports string) ClientPortProbeDeps {
	t.Helper()
	return ClientPortProbeDeps{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" {
			t.Fatal(name)
		}
		switch {
		case args[0] == "context":
			return []byte("unix:///var/run/docker.sock\n"), nil
		case args[0] == "ps":
			if !strings.Contains(strings.Join(args, " "), "label=k3d.cluster=example") {
				t.Fatal(args)
			}
			return []byte("abc123\n"), nil
		case args[0] == "inspect" && args[2] == `{{index .Config.Labels "k3d.cluster"}}`:
			if args[3] != "k3d-example-server-0" {
				return nil, errors.New("not this plan's node")
			}
			return []byte("example\n"), nil
		case args[0] == "inspect" && args[2] == "{{json .NetworkSettings.Ports}}":
			return []byte(ports), nil
		default:
			t.Fatalf("unexpected inspection: %v", args)
			return nil, errors.New("unexpected")
		}
	}, Dial: func(context.Context, string) error { return errors.New("server not installed yet") }}
}

const forwardedProbePorts = `{"30443/tcp":[{"HostIp":"0.0.0.0","HostPort":"32443"},{"HostIp":"::","HostPort":"32443"}],"30022/tcp":[{"HostIp":"127.0.0.1","HostPort":"32022"}]}`

func TestClientPortProbeProposesSelectedClusterMappings(t *testing.T) {
	result, err := ProbeClientPorts(t.Context(), probePlan(), Config{}, probeDocker(t, forwardedProbePorts))
	if err != nil {
		t.Fatal(err)
	}
	if result.HTTPS.Port != "32443" || result.SSH.Port != "32022" || !result.HTTPS.Published || !result.SSH.Published || result.NeedsAction || result.HTTPS.Reachable || result.SSH.Reachable {
		t.Fatalf("%+v", result)
	}
	if !reflect.DeepEqual(result.InstallFlags, []string{"--client-host", "localhost", "--client-https-port", "32443", "--client-ssh-port", "32022"}) {
		t.Fatal(result.InstallFlags)
	}
	if !strings.Contains(result.Advice, "do not prove Oberth readiness or TLS identity") {
		t.Fatal(result.Advice)
	}
}

func TestClientPortProbeExplicitPortsOverrideDiscovery(t *testing.T) {
	for _, tc := range []struct {
		https, ssh string
		mapped     bool
	}{{"32443", "32022", true}, {"31443", "31022", false}} {
		result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHTTPSPort: tc.https, ClientSSHPort: tc.ssh}, probeDocker(t, forwardedProbePorts))
		if err != nil || result.HTTPS.Port != tc.https || result.SSH.Port != tc.ssh || result.HTTPS.Published != tc.mapped || result.SSH.Published != tc.mapped {
			t.Fatalf("%+v %v", result, err)
		}
	}
}

func TestClientPortProbeNeedsNoClusterTypeOrDocker(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		deps := ClientPortProbeDeps{Run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no Docker") }, Dial: func(context.Context, string) error {
			if reachable {
				return nil
			}
			return errors.New("closed")
		}}
		result, err := ProbeClientPorts(t.Context(), probePlan(), Config{}, deps)
		if err != nil || result.HTTPS.Port != "30443" || result.SSH.Port != "30022" || result.NeedsAction == reachable {
			t.Fatalf("%+v %v", result, err)
		}
		if !reachable && !strings.Contains(result.Advice, "NodePorts are not reachable from this host; map them or use port-forward") {
			t.Fatal(result.Advice)
		}
	}
}

func TestClientPortProbeDoesNotBorrowRemoteOrOtherClusterBindings(t *testing.T) {
	for _, mode := range []string{"remote daemon", "remote context override", "other plan", "ambiguous mapping", "other interface"} {
		t.Run(mode, func(t *testing.T) {
			deps := probeDocker(t, forwardedProbePorts)
			plan := probePlan()
			switch mode {
			case "remote daemon":
				deps.DockerHost = "ssh://remote.example.test"
				deps.Run = func(context.Context, string, ...string) ([]byte, error) {
					t.Fatal("remote daemon must not be inspected")
					return nil, nil
				}
			case "remote context override":
				deps.DockerHost = "unix:///var/run/docker.sock"
				deps.DockerContext = "remote"
				deps.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
					if args[0] != "context" || args[len(args)-1] != "remote" {
						t.Fatal(args)
					}
					return []byte("ssh://remote.example.test"), nil
				}
			case "other plan":
				plan.Identity.Nodes[0].Name = "different-node"
			case "ambiguous mapping":
				deps = probeDocker(t, `{"30443/tcp":[{"HostIp":"0.0.0.0","HostPort":"32443"},{"HostIp":"0.0.0.0","HostPort":"31443"}]}`)
			case "other interface":
				deps = probeDocker(t, `{"30443/tcp":[{"HostIp":"192.0.2.1","HostPort":"32443"}]}`)
			}
			result, err := ProbeClientPorts(t.Context(), plan, Config{}, deps)
			if err != nil || result.HTTPS.Port != "30443" || result.HTTPS.Published || !result.NeedsAction {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestClientPortProbeUsesRealTCPWithoutTrustingTheListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	deps := DefaultClientPortProbeDeps()
	deps.Run = nil
	result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: "localhost", ClientHTTPSPort: port, ClientSSHPort: port}, deps)
	if err != nil || !result.HTTPS.Reachable || !result.SSH.Reachable || result.NeedsAction || result.HTTPS.Published {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestClientPortProbeRefusesInvalidInputsBeforeInspection(t *testing.T) {
	for _, cfg := range []Config{{ClientHTTPSPort: "0"}, {ClientSSHPort: "65536"}, {ClientHTTPSPort: "0443"}, {ClientHost: "host;command"}, {ServiceType: "arbitrary"}} {
		deps := ClientPortProbeDeps{Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("inspected invalid input")
			return nil, nil
		}}
		if _, err := ProbeClientPorts(t.Context(), probePlan(), cfg, deps); err == nil {
			t.Fatal(cfg)
		}
	}
	plan := probePlan()
	plan.Identity.Context = ""
	if _, err := ProbeClientPorts(t.Context(), plan, Config{}, ClientPortProbeDeps{}); err == nil {
		t.Fatal("accepted missing context")
	}
}
