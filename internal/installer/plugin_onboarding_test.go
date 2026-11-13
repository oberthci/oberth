package installer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

func TestPluginDNSRemedyWaitsForNameAndReprobes(t *testing.T) {
	for _, address := range []string{"192.0.2.10", "2001:db8::10", "edge.example.test"} {
		t.Run(address, func(t *testing.T) {
			configured := false
			deps := ClientPortProbeDeps{EndpointAddress: address, LookupHost: func(_ context.Context, host string) ([]string, error) {
				if host == address || configured {
					return []string{"192.0.2.10", "2001:db8::10"}, nil
				}
				return nil, errors.New("not configured")
			}}
			host := "oberth.test"
			result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: host}, deps)
			if err != nil || result.DNS == nil || result.DNS.Resolves || !result.NeedsAction || result.Host != "oberth.test" {
				t.Fatalf("%+v %v", result, err)
			}
			if !strings.Contains(result.DNS.DNSRecord, address) || !strings.Contains(result.Advice, "wait for the user") {
				t.Fatal(result)
			}
			if address != "edge.example.test" && result.DNS.HostsLine != address+" oberth.test" {
				t.Fatal(result.DNS)
			}
			configured = true
			deps.LookupHost = func(_ context.Context, name string) ([]string, error) {
				if address == "edge.example.test" {
					return []string{"192.0.2.10"}, nil
				}
				return []string{address}, nil
			}
			result, err = ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: result.Host}, deps)
			if err != nil || !result.DNS.Resolves {
				t.Fatalf("name not re-probed: %+v %v", result, err)
			}
		})
	}
}

func TestPluginDNSRejectsStaleOrInvalidAddresses(t *testing.T) {
	deps := ClientPortProbeDeps{EndpointAddress: "192.0.2.10", LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10", "192.0.2.11"}, nil }}
	result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: "chosen.example.test"}, deps)
	if err != nil || result.DNS.Resolves {
		t.Fatalf("%+v %v", result, err)
	}
	deps.EndpointAddress = "host;command"
	if _, err := ProbeClientPorts(t.Context(), probePlan(), Config{}, deps); err == nil {
		t.Fatal("accepted invalid endpoint")
	}
}

func TestPluginOffersLoadBalancerFromGenericReachability(t *testing.T) {
	plan := probePlan()
	plan.Identity.Nodes[0].Addresses = []string{"192.0.2.20"}
	for _, reachable := range []bool{false, true} {
		deps := ClientPortProbeDeps{Dial: func(_ context.Context, address string) error {
			if reachable && strings.HasPrefix(address, "192.0.2.20:") {
				return nil
			}
			return errors.New("unreachable")
		}}
		result, err := ProbeClientPorts(t.Context(), plan, Config{}, deps)
		if err != nil || (result.LoadBalancer != nil) == reachable {
			t.Fatalf("%+v %v", result, err)
		}
	}
}

func TestPluginWaitLoadBalancerIsBoundedAndSelected(t *testing.T) {
	kube := fake.NewSimpleClientset()
	calls := 0
	kube.PrependReactor("get", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != "selected-namespace" || action.(k8stesting.GetAction).GetName() != "oberth" {
			t.Fatal(action)
		}
		calls++
		service := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
		if calls > 1 {
			service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "edge.example.test"}}
		}
		return true, service, nil
	})
	address, err := waitClientLoadBalancer(t.Context(), kube, "selected-namespace", time.Millisecond)
	if err != nil || address != "edge.example.test" || calls != 2 {
		t.Fatalf("%s %v (%d calls)", address, err, calls)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	_, err = waitClientLoadBalancer(ctx, fake.NewSimpleClientset(), "selected-namespace", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "inspect Service") {
		t.Fatal(err)
	}
}

func TestPluginKindPrintsCompleteRemedyBeforeInstall(t *testing.T) {
	plan := probePlan()
	plan.Identity.Distribution = "kind"
	result, err := ProbeClientPorts(t.Context(), plan, Config{ClientHTTPSPort: "32443", ClientSSHPort: "32022"}, ClientPortProbeDeps{})
	if err != nil || result.KindRemedy == nil {
		t.Fatalf("%+v %v", result, err)
	}
	var config struct {
		Kind       string
		APIVersion string
		Nodes      []struct {
			ExtraPortMappings []struct {
				ContainerPort, HostPort int
				ListenAddress, Protocol string
			}
		}
	}
	if err := yaml.Unmarshal([]byte(result.KindRemedy.Config), &config); err != nil {
		t.Fatal(err)
	}
	if config.Kind != "Cluster" || config.APIVersion != "kind.x-k8s.io/v1alpha4" || len(config.Nodes) != 1 {
		t.Fatalf("%+v", config)
	}
	ports := config.Nodes[0].ExtraPortMappings
	if len(ports) != 2 || ports[0].ContainerPort != 30022 || ports[0].HostPort != 32022 || ports[1].ContainerPort != 30443 || ports[1].HostPort != 32443 || ports[0].ListenAddress != "127.0.0.1" || ports[1].Protocol != "TCP" {
		t.Fatal(ports)
	}
	if result.KindRemedy.CreateCommand != "test ! -e oberth-kind.kubeconfig && test ! -L oberth-kind.kubeconfig && kind create cluster --name oberth-onboarding --config oberth-kind.yaml --kubeconfig oberth-kind.kubeconfig" {
		t.Fatal(result.KindRemedy)
	}
}

func TestPluginKindRecognizesSelectedClusterMappings(t *testing.T) {
	plan := probePlan()
	plan.Identity.Distribution = "kind"
	plan.Identity.Nodes[0].Name = "chosen-control-plane"
	deps := ClientPortProbeDeps{DockerHost: "unix:///var/run/docker.sock", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "inspect" && args[2] == `{{index .Config.Labels "k3d.cluster"}}`:
			return nil, errors.New("not k3d")
		case args[0] == "inspect" && args[2] == `{{index .Config.Labels "io.x-k8s.kind.cluster"}}`:
			if args[3] != "chosen-control-plane" {
				t.Fatal(args)
			}
			return []byte("chosen"), nil
		case args[0] == "ps":
			if args[2] != "label=io.x-k8s.kind.cluster=chosen" {
				t.Fatal(args)
			}
			return []byte("abc123"), nil
		case args[0] == "inspect" && args[3] == "abc123":
			return []byte(forwardedProbePorts), nil
		default:
			t.Fatal(args)
			return nil, errors.New("unexpected command")
		}
	}}
	result, err := ProbeClientPorts(t.Context(), plan, Config{}, deps)
	if err != nil || result.KindRemedy != nil || !result.HTTPS.Published || !result.SSH.Published || result.HTTPS.Port != "32443" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestPluginPrerequisitesOnlyPrintMissingToolCommands(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/bin/zsh"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		for _, platform := range []string{"Darwin", "Linux"} {
			for _, tools := range []string{"", "kubectl", "helm", "kubectl helm"} {
				t.Run(filepath.Base(shell)+"/"+platform+"/"+tools, func(t *testing.T) {
					root := t.TempDir()
					bin := filepath.Join(root, "bin")
					if err := os.Mkdir(bin, 0700); err != nil {
						t.Fatal(err)
					}
					write := func(name, body string) {
						t.Helper()
						if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
							t.Fatal(err)
						}
					}
					write("uname", "printf '%s\\n' '"+platform+"'\n")
					write("brew", "printf called > \"$HOME/brew-calls\"\nexit 91\n")
					for _, tool := range strings.Fields(tools) {
						write(tool, "exit 0\n")
					}
					command := exec.CommandContext(t.Context(), shell, pluginPath(t, "scripts", "install"), "prerequisites")
					command.Env = []string{"HOME=" + root, "PATH=" + bin}
					command.Stdin = strings.NewReader("yes\nyes\n")
					output, err := command.CombinedOutput()
					if (err == nil) != (tools == "kubectl helm") {
						t.Fatalf("%v %s", err, output)
					}
					if _, err := os.Stat(filepath.Join(root, "brew-calls")); !os.IsNotExist(err) {
						t.Fatal("prerequisite validation ran brew", err)
					}
					for tool, formula := range map[string]string{"kubectl": "kubernetes-cli", "helm": "helm"} {
						missing := !strings.Contains(tools, tool)
						if strings.Contains(string(output), "Install "+tool+" first") != missing {
							t.Fatal(string(output))
						}
						if strings.Contains(string(output), "brew install "+formula) != (missing && platform == "Darwin") {
							t.Fatal(string(output))
						}
					}
					if strings.Contains(string(output), "Type yes") || strings.Contains(string(output), "offer to run") {
						t.Fatal("validation offered to install dependencies", string(output))
					}
				})
			}
		}
	}
}

func TestPluginPrerequisitesRejectInstallationOption(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", pluginPath(t, "scripts", "install"), "prerequisites", "--install-missing")
	command.Env = []string{"HOME=" + t.TempDir(), "PATH=" + t.TempDir()}
	command.Stdin = strings.NewReader("yes\nyes\n")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Usage: scripts/install prerequisites") {
		t.Fatalf("%v %s", err, output)
	}
}

func TestPluginLoadBalancerWaitUsesPlanFilesAndContext(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/chosen/services/oberth" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Service","spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{"ingress":[{"ip":"192.0.2.10"}]}}}`))
	}))
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config")
	config := clientcmdapi.NewConfig()
	config.Clusters["chosen"] = &clientcmdapi.Cluster{Server: server.URL}
	config.Clusters["wrong"] = &clientcmdapi.Cluster{Server: "http://127.0.0.1:1"}
	config.Contexts["chosen-context"] = &clientcmdapi.Context{Cluster: "chosen"}
	config.Contexts["wrong-context"] = &clientcmdapi.Context{Cluster: "wrong"}
	config.CurrentContext = "wrong-context"
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(root, "unrelated"))
	plan := probePlan()
	plan.Identity.Files = []string{path}
	plan.Identity.Context = "chosen-context"
	address, err := WaitClientLoadBalancer(t.Context(), plan, "chosen")
	if err != nil || address != "192.0.2.10" || requests.Load() != 1 {
		t.Fatalf("%s %v (%d requests)", address, err, requests.Load())
	}
	plan.Identity.Files = nil
	if _, err := WaitClientLoadBalancer(t.Context(), plan, "chosen"); err == nil {
		t.Fatal("used ambient kubeconfig instead of the selected plan")
	}
}
