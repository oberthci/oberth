package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestPrintedRoutingRemedyRestoresBothVerifiedEndpoints(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	address := server.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	server.Listener.Close()
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sshListener.Close()
	_, sshPort, _ := net.SplitHostPort(sshListener.Addr().String())
	kube := fake.NewClientset(preflightNode(), preflightStorage(), readyOberthPod(), readyArgoControllerPod())
	ca := newTestServerCA(t, []string{"api.example.test"})
	var output bytes.Buffer
	deployed, verifying, applies := false, false, 0
	host := InstallDeps{Output: &output, LookPath: func(s string) (string, error) { return s, nil }, ProbeNetworkPolicy: enforcedPolicyFixture,
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			return kube, &rest.Config{Host: "https://192.0.2.10:6443", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
		},
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "port-forward") || strings.Contains(joined, "config set") {
				t.Fatalf("user routing/config action executed: %s", joined)
			}
			if strings.Contains(joined, "upstream list --check") {
				return []byte("NAME   KIND URL                              AUTH\nforge  ssh  ssh://git@forge.example.test/acme      ok\n"), nil
			}
			if strings.Contains(joined, "upstream list") {
				return []byte("NAME KIND URL KEY FINGERPRINT\nforge ssh ssh://git@forge.example.test/acme SHA256:public\n"), nil
			}
			if strings.Contains(joined, "admin read-file") && verifying {
				if cert := server.Certificate(); cert != nil {
					return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
				}
				return ca, nil
			}
			if verifying && (strings.Contains(joined, "uplink add") || strings.Contains(joined, "upstream add") || strings.Contains(joined, "enroll")) {
				t.Fatalf("verify repeated onboarding: %s", joined)
			}
			return preflightStats(ctx, input, name, args...)
		},
		RunHelm: func(ctx context.Context, args []string) ([]byte, error) {
			joined := " " + strings.Join(args, " ") + " "
			if strings.Contains(joined, " list ") {
				if deployed {
					return []byte(`[{"name":"oberth"}]`), nil
				}
				return []byte("[]"), nil
			}
			if strings.Contains(joined, " show ") {
				return []byte(fakeChartValuesYAML), nil
			}
			if strings.Contains(joined, " get values ") {
				return []byte(`{"networkPolicy":{"enabled":true}}`), nil
			}
			if strings.Contains(joined, " upgrade ") && strings.Contains(joined, " oberth ") && !strings.Contains(joined, "--dry-run") {
				if verifying {
					t.Fatal("verify deployed again")
				}
				applies++
				deployed = true
				replicas := int32(1)
				_, err := kube.AppsV1().Deployments(DefaultNamespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				_, err = kube.CoreV1().Services(DefaultNamespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 8443}, {Name: "ssh", Port: 2223}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			return nil, nil
		}}
	cfg := Config{ContextName: "chosen", ChartPath: testLocalChart, ImageRef: fakeChartImageRef, ClientHost: "127.0.0.1", ClientHTTPSPort: port, ClientSSHPort: sshPort, ClientAccess: "none", Timeout: time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	err = ExecuteJSON(ctx, cfg, host)
	if err == nil || !deployed {
		t.Fatalf("not deployed failure: %v, %s", err, output.String())
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("client check ignored cancellation bound")
	}
	var failure InstallResult
	if err := json.Unmarshal(output.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if len(failure.Checks) != 1 || failure.Checks[0].Code != "client_bootstrap_endpoint" {
		t.Fatalf("wrong failure: %+v", failure)
	}
	// The actual --verify --json path must report the same endpoint check and
	// exact remedy as ExecuteJSON, while retaining SSH's own check identity.
	verifying = true
	output.Reset()
	if err := PrintVerification(t.Context(), cfg, host, true); err == nil {
		t.Fatal("unreachable endpoints verified")
	}
	var verified InstallResult
	if err := json.Unmarshal(output.Bytes(), &verified); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, check := range verified.Checks {
		if check.Code == "client_bootstrap_endpoint" || check.Code == "ssh_reachable" {
			seen++
			if check.OK || check.Remedy != failure.Checks[0].Remedy || !strings.Contains(check.Reason, "connection refused") {
				t.Fatalf("install/verify endpoint contract differs: %+v versus %+v", check, failure.Checks[0])
			}
		}
		if check.Code == "https_tls" {
			t.Fatal("verify retained a second HTTPS endpoint code", check)
		}
	}
	if seen != 2 {
		t.Fatal("missing HTTPS/SSH endpoint failure checks", verified.Checks)
	}
	certPath := filepath.Join(t.TempDir(), "client.csr.crt")
	if err := os.WriteFile(certPath, []byte("already issued public certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	remedy := failure.Checks[0].Remedy
	route := strings.Split(strings.Split(remedy, "port-forward service/oberth ")[1], ")")[0]
	mappings := strings.Fields(route)
	review := Review(cfg)
	if len(mappings) != 2 || !strings.Contains(strings.Join(review.ClientRequirements, " "), route) {
		t.Fatal("Review/recovery do not advertise the same two routes", remedy, review)
	}
	service, err := kube.CoreV1().Services(DefaultNamespace).Get(t.Context(), "oberth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mapped := map[string]string{}
	for _, mapping := range mappings {
		local, target, ok := strings.Cut(mapping, ":")
		if !ok {
			t.Fatal(mapping)
		}
		for _, p := range service.Spec.Ports {
			if target == p.Name {
				mapped[p.Name] = local
			}
		}
	}
	if mapped["https"] != port || mapped["ssh"] != sshPort {
		t.Fatal("printed mappings do not resolve to the custom Service's ports", route)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", mapped["https"]))
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	verifying = true
	// These are precisely the endpoint/context flags printed in the remedy.
	verifyCfg := Config{ContextName: "chosen", Namespace: DefaultNamespace, ClientHost: "127.0.0.1", ClientHTTPSPort: port, ClientSSHPort: sshPort}
	onlyHTTPS := VerifyInstall(t.Context(), verifyCfg, host)
	var failed []string
	for _, check := range onlyHTTPS.Checks {
		if !check.OK && !check.Warning {
			failed = append(failed, check.Code)
		}
	}
	if strings.Join(failed, ",") != "ssh_reachable" {
		t.Fatalf("HTTPS-only remedy unexpected failures: %+v", onlyHTTPS.Checks)
	}
	for _, check := range onlyHTTPS.Checks {
		if check.Code == "ssh_reachable" {
			if !strings.Contains(check.Reason, "connection refused") || !strings.Contains(check.Reason, sshPort) || !strings.Contains(check.Remedy, route) {
				t.Fatal("SSH failure lacks observed error and complete routing remedy", check)
			}
		}
	}
	sshListener, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", mapped["ssh"]))
	if err != nil {
		t.Fatal(err)
	}
	defer sshListener.Close()
	got := VerifyInstall(t.Context(), verifyCfg, host)
	if !got.OK {
		t.Fatalf("verify did not recover: %+v", got.Checks)
	}
	after, err := os.ReadFile(certPath)
	if err != nil || string(after) != "already issued public certificate" || applies != 1 {
		t.Fatal("verification changed existing state")
	}
	t.Logf("failure and recovery passed; initial gate took %s; endpoint %s", time.Since(start), got.ServerURL)
}
