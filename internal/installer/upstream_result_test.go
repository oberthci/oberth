package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestInstallJSONPendingUpstreamCarriesAction(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	keyLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for _, tc := range []struct {
		name                        string
		ready, existing, authorized bool
		additionalFailure           bool
	}{
		{"action_needed", true, false, false, false}, {"real_failure", false, false, false, false},
		{"verified", true, true, true, false}, {"existing_pending", true, true, false, false},
		{"additional_registration_failure", true, false, false, true},
		{"existing_additional_registration_failure", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := tc.ready
			replicas := int32(1)
			readyReplicas := int32(0)
			if ready {
				readyReplicas = 1
			}
			kube := fake.NewClientset(preflightNode(), preflightStorage(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}}, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas}})
			var output bytes.Buffer
			host := testAdminInstallDeps(InstallDeps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
					return []byte(`[{"name":"oberth"}]`), nil
				}
				return []byte(`{"networkPolicy":{"enabled":true}}`), nil
			}, ProbeNetworkPolicy: enforcedPolicyFixture, Output: &output, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
				return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "selected", nil
			}, RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				command := strings.Join(args, " ")
				if strings.Contains(command, "admin read-file") {
					return ca, nil
				}
				if strings.Contains(command, "admin readiness") {
					return []byte(`{"status":"not ready","code":"no_upstream"}`), nil
				}
				if tc.existing && strings.Contains(command, "upstream list --check") {
					status := "ok"
					if !tc.authorized {
						status = "pending: the forge has not accepted this deploy key"
					}
					return []byte(fmt.Sprintf("%-8s%-6s%-36s%s\n%-8s%-6s%-36s%s\n", "NAME", "KIND", "URL", "AUTH", "github", "ssh", "ssh://git@github.com/acme", status)), nil
				}
				t.Fatal("pending install probed forge authentication")
				return nil, nil
			}})
			cfg := Config{ForgeType: "github", ForgeOrg: "acme", ClientAccess: "none", ClientHost: "127.0.0.1", ClientHTTPSPort: port, ClientSSHPort: port, ContextName: "selected", KubeconfigPath: "/secure/kube config", UplinkIdentity: "admin@workstation", uplinkOnboarded: true, SSHPublicKeyPath: "-"}
			if tc.additionalFailure {
				cfg.AlsoUpstreams = []string{"ssh://git@codeberg.org/another-org"}
			}
			// Kubeconfig loading is injected; the command still carries its exact file.
			verifyCfg := cfg
			verifyCfg.KubeconfigPath = ""
			err := executeJSON(t.Context(), verifyCfg, host, func(ctx context.Context, got Config, deps InstallDeps) error {
				got.KubeconfigPath = cfg.KubeconfigPath
				if err := got.Validate(); err != nil {
					return err
				}
				pod := &fakeOberthHost{t: t, addResults: []fakeAddResult{{registers: true, out: keyLine + "\nUpstream github registered; deploy key pending.\n"}}}
				if tc.existing {
					pod.upstreamRows = []string{"github ssh ssh://git@github.com/acme SHA256:public"}
				}
				if tc.additionalFailure {
					failure := fakeAddResult{out: "host key mismatch", err: errors.New("upstream registration failed")}
					if tc.existing {
						pod.addResults = []fakeAddResult{failure}
					} else {
						pod.addResults = append(pod.addResults, failure)
					}
				}
				var discarded bytes.Buffer
				onboard := onboardingDeps(t, pod, &discarded, strings.NewReader(""), false)
				onboard.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
					if strings.Contains(strings.Join(args, " "), "admin read-file /run/oberth-identities/upstream/id_ed25519.pub") {
						return []byte(keyLine), nil
					}
					return pod.run(ctx, input, name, args...)
				}
				refreshFixtureAdmin(&onboard)
				onboard.Output = deps.Output
				onboard.ContextName = "selected"
				onboard.upstreamResult = deps.upstreamResult
				tw := newTableWriter(deps.Output, false)
				tw.WriteHeader()
				tw.WriteFooter()
				return FinishInstall(ctx, got, onboard, tw, &heldCredentials{})
			})
			wantOK := tc.authorized && !tc.additionalFailure
			if errors.Is(err, ErrUpstreamKeyPending) != (ready && !tc.authorized && !tc.additionalFailure) || (err == nil) != wantOK {
				t.Fatal("wrong pending/failure classification")
			}
			var result InstallResult
			if json.Unmarshal(output.Bytes(), &result) != nil || result.OK != wantOK || result.Upstream == nil {
				t.Fatal("invalid JSON partial result")
			}
			if result.ClientAccess == nil || result.ClientAccess.Authentication != "mutual_tls" || result.ClientAccess.TokenRequired || !strings.Contains(result.ClientAccess.NextStep, "/oberth:connect") {
				t.Fatal("JSON result lacks certificate connection guidance", result.ClientAccess)
			}
			u := result.Upstream
			wantState := "pending"
			if tc.authorized {
				wantState = "verified"
			}
			if u.Name != "github" || u.URL != "ssh://git@github.com/acme" || u.State != wantState || u.DeployPublicKey != keyLine || u.Fingerprint != ssh.FingerprintSHA256(key) || u.ForgeSettingsURL != "https://github.com/settings/keys" {
				t.Fatal("missing public upstream action")
			}
			if !strings.HasPrefix(u.NextCommand, "oberth ") || !strings.Contains(u.NextCommand, "upstream add --yes github ssh://git@github.com/acme") || strings.Contains(u.NextCommand, "--no-wait") {
				t.Fatal("next command lost its target or skipped verification")
			}
			pending := false
			for _, check := range result.Checks {
				if check.Code == "upstream_key_pending" {
					pending = !check.OK && check.DeployPublicKey == keyLine && strings.Contains(check.Remedy, "write access")
				}
			}
			if pending == tc.authorized {
				t.Fatal("incorrect pending check or missing inline key")
			}
		})
	}
}

func TestInstallSkillUsesPublicUpstreamAction(t *testing.T) {
	raw, err := os.ReadFile("../../claude-plugin/skills/install/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"upstream.deploy_public_key", "upstream.forge_settings_url", "upstream.next_command", "upstream_key_pending", "exit code 3", "/oberth:add-repo", "machine user"} {
		if !strings.Contains(text, required) {
			t.Fatalf("skill missing %s", required)
		}
	}
	if strings.Contains(text, "When a deploy key is printed") {
		t.Fatal("skill still relies on discarded text output")
	}
}

func TestInstallUpstreamResultRejectsPrivateMaterial(t *testing.T) {
	result := installUpstreamResult(Config{}, Deps{}, "custom", "ssh://git@forge.example/acme", "-----BEGIN PRIVATE KEY-----\ninvalid", false)
	if result.DeployPublicKey != "" || result.Fingerprint != "" || result.ForgeSettingsURL != "" {
		t.Fatal("untrusted material entered public result")
	}
	var output bytes.Buffer
	if err := json.NewEncoder(&output).Encode(result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("private material disclosed")
	}
}
