package installer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
			if address != "edge.example.test" {
				host = address
			}
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
