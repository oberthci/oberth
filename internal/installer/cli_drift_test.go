package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestClusterIPBannerUsesServicePort(t *testing.T) {
	args := strings.Join(OberthHelmArgs(Config{ServiceType: "ClusterIP", ClientHost: "oberth.internal"}, OpenBaoResult{}, RekorResult{}), " ")
	if !strings.Contains(args, "pushBannerURL=https://oberth.internal:443/runs") || strings.Contains(args, ":30443/runs") {
		t.Fatal(args)
	}
}
func TestRolloutCancellationPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deps := Deps{KubeClient: fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth", Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}}}), Output: &bytes.Buffer{}}
	err := waitForUpgradeRollout(ctx, deps, UpgradeConfig{Namespace: "oberth", Timeout: time.Minute}, "fixture")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("cancelled rollout = %v", err)
	}
}
func TestPodCancellationPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deps := Deps{KubeClient: fake.NewClientset(), Output: &bytes.Buffer{}}
	err := waitForPodReady(ctx, deps, "oberth", "app=fixture", time.Minute)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("cancelled pod wait = %v", err)
	}
}

func TestInstalledServiceControlsClientEndpoints(t *testing.T) {
	for _, tc := range []struct {
		kind, host, ssh, https string
		nodeSSH, nodeHTTPS     int32
	}{
		{"ClusterIP", "oberth.oberth.svc", "2222", "443", 0, 0},
		{"NodePort", "node.example", "31022", "31443", 31022, 31443},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceType(tc.kind), Ports: []corev1.ServicePort{{Name: "ssh", Port: 2222, NodePort: tc.nodeSSH}, {Name: "https", Port: 443, NodePort: tc.nodeHTTPS}}}}
			deps, err := configureClientEndpoint(t.Context(), "oberth", tc.kind, Deps{KubeClient: fake.NewClientset(svc), RestConfig: &rest.Config{Host: "https://node.example:6443"}})
			if err != nil {
				t.Fatal(err)
			}
			ssh, https := clientPorts(deps)
			if clientReachableHost(deps) != tc.host || ssh != tc.ssh || https != tc.https {
				t.Fatalf("endpoint: %+v %s %s", deps, ssh, https)
			}
			text := strings.Join(NextSteps(NextStepsInput{Host: clientReachableHost(deps), SSHPort: ssh, HTTPSPort: https, ServiceType: tc.kind, Upstreams: []NextStepsUpstream{{Name: "forge", BaseURL: "ssh://git@forge.example/org", Connected: true}}}), "\n")
			if !strings.Contains(text, "ssh://git@"+tc.host+":"+ssh) || !strings.Contains(text, "https://"+tc.host+":"+https) {
				t.Fatal(text)
			}
			if tc.kind == "ClusterIP" && !strings.Contains(text, "port-forward service/oberth 30022:2222 30443:443") {
				t.Fatal(text)
			}
		})
	}
}

func TestManualClusterIPNextSteps(t *testing.T) {
	var out bytes.Buffer
	if err := PrintNextSteps(Config{Namespace: "private", ServiceType: "ClusterIP"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"port-forward service/oberth 30022:2222 30443:443", "ssh://git@oberth.private.svc:2222"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, &out)
		}
	}
}
func TestLoadBalancerEndpointKeepsConfiguredTLSHost(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "ssh", Port: 2222, NodePort: 31022}, {Name: "https", Port: 443, NodePort: 31443}}}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}}}}
	node := WithServiceEndpoint(svc, "oberth", Deps{RestConfig: &rest.Config{Host: "https://192.0.2.20:6443"}})
	if node.ClientHost != "192.0.2.20" || node.ClientHTTPSPort != "31443" {
		t.Fatalf("unconfigured ingress replaced the TLS-bound node endpoint: %+v", node)
	}
	external := WithServiceEndpoint(svc, "oberth", Deps{ClientHost: "oberth.example"})
	if external.ClientHost != "oberth.example" || external.ClientHTTPSPort != "443" || external.ClientSSHPort != "2222" {
		t.Fatalf("configured front door: %+v", external)
	}
}
