package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// Exercise Execute/ExecuteJSON through preflight, Helm, and FinishInstall;
// only the Kubernetes and subprocess transports are simulated.
func TestInstallUnreachableClientEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ca := newTestServerCA(t, []string{"api.example.test"})
	for _, jsonOutput := range []bool{false, true} {
		t.Run(strconv.FormatBool(jsonOutput), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			_ = listener.Close()
			kube := fake.NewClientset(preflightNode(), preflightStorage(), readyOberthPod(), readyArgoControllerPod())
			var output bytes.Buffer
			deployed := false
			host := testAdminInstallDeps(InstallDeps{Output: &output, LookPath: func(name string) (string, error) { return name, nil }, ProbeNetworkPolicy: enforcedPolicyFixture,
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://192.0.2.10:6443", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
					if slices.Contains(args, "create-first-admin") {
						return (&fakeOberthHost{t: t}).run(ctx, input, name, args...)
					}
					if strings.Contains(strings.Join(args, " "), "upstream list") {
						return []byte("NAME KIND URL KEY FINGERPRINT\nforge ssh ssh://git@forge.example.test/repos SHA256:public\n"), nil
					}
					return preflightStats(ctx, input, name, args...)
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					joined := " " + strings.Join(args, " ") + " "
					if strings.Contains(joined, " list ") {
						return []byte("[]"), nil
					}
					if strings.Contains(joined, " show ") {
						return []byte(fakeChartValuesYAML), nil
					}
					if strings.Contains(joined, " get values ") {
						return []byte(`{}`), nil
					}
					if strings.Contains(joined, " upgrade ") && strings.Contains(joined, "oberth") {
						deployed = true
					}
					return nil, nil
				},
			})
			cfg := Config{ContextName: "chosen", ChartPath: testLocalChart, ImageRef: fakeChartImageRef, ClientHost: "localhost", ClientHTTPSPort: port, ClientAccess: "none", Timeout: time.Second}
			if jsonOutput {
				err = ExecuteJSON(t.Context(), cfg, host)
			} else {
				err = Execute(t.Context(), cfg, host)
			}
			if err == nil || !deployed {
				t.Fatalf("did not reach deployed endpoint failure: %v\n%s", err, output.String())
			}
			text := output.String() + err.Error()
			if jsonOutput {
				var result InstallResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.OK {
					t.Fatal(output.String())
				}
				if len(result.Checks) != 1 || result.Checks[0].Code != "client_bootstrap_endpoint" {
					t.Fatalf("%+v", result.Checks)
				}
			}
			for _, want := range []string{"client bootstrap", "https://localhost:" + port, "connection refused", "port-forward", "--client-host", "remain deployed", "Resume", "--resume-secrets-file", "--verify --json", "without reissuing the client certificate"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
		})
	}
}

func TestReviewClientRoutingRequirements(t *testing.T) {
	if review := Review(Config{ServiceType: "ClusterIP"}); len(review.ClientRequirements) == 0 {
		t.Fatal("ClusterIP routing requirement missing before host selection")
	}
	for _, host := range []string{"localhost", "::1", "oberth.example.test"} {
		var text bytes.Buffer
		PrintReview(&text, Config{ClientHost: host, ContextName: "chosen", Namespace: "oberth"})
		review := Review(Config{ClientHost: host})
		if !strings.Contains(text.String(), "Required before client bootstrap") || len(review.ClientRequirements) == 0 {
			t.Fatal(text.String(), review)
		}
	}
}

func TestFreshInstallChecksEndpointAfterRegistrationMakesServiceReady(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(reserved.Addr().(*net.TCPAddr).Port)
	_ = reserved.Close()
	var listener net.Listener
	t.Cleanup(func() {
		if listener != nil {
			_ = listener.Close()
		}
	})
	pod := &fakeOberthHost{t: t}
	key, _ := stageUplinkKeyPair(t)
	var output bytes.Buffer
	deps := onboardingDeps(t, pod, &output, strings.NewReader(""), false)
	deps.CredentialSink = func(string, string) {}
	deps.checkClientReachability = true
	running, err := deps.KubeClient.CoreV1().Pods(DefaultNamespace).Get(t.Context(), "oberth-7f9", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	running.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := deps.KubeClient.CoreV1().Pods(DefaultNamespace).UpdateStatus(t.Context(), running, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deps.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		out, err := pod.run(ctx, input, name, args...)
		if err == nil && strings.Contains(strings.Join(args, " "), "upstream add") {
			// Model the real Service: no public listener until registration has
			// made the Pod Ready. An early endpoint gate can never reach this step.
			listener, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				t.Fatal(err)
			}
			running.Status.Conditions[0].Status = corev1.ConditionTrue
			if _, err := deps.KubeClient.CoreV1().Pods(DefaultNamespace).UpdateStatus(ctx, running, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return out, err
	}
	refreshFixtureAdmin(&deps)
	cfg := Config{ForgeType: "github", ForgeOrg: "acme", SSHPublicKeyPath: key, UplinkIdentity: "admin@workstation", ClientAccess: "none", ClientHost: "127.0.0.1", ClientHTTPSPort: port, Timeout: time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	err = finishInstallTest(t.Context(), cfg, deps, &output)
	if err != nil && !errors.Is(err, ErrUpstreamKeyPending) {
		t.Fatal(err)
	}
	if pod.addCalls != 1 || listener == nil || !strings.Contains(output.String(), "Ready") {
		t.Fatal("fresh onboarding did not make the endpoint reachable", output.String())
	}
}
