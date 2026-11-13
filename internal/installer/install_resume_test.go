package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
)

func TestInstallJSONOnboardingAndResume(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("FirstRunPending=%t", pending), func(t *testing.T) {
			public, _ := newEd25519HostKey(t)
			key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
			registered := false
			calls := map[string]int{}
			ca := ""
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := strings.TrimPrefix(r.URL.Path, "/api/admin/")
				calls[action]++
				w.Header().Set("Content-Type", "application/json")
				var value any = map[string]any{}
				switch action {
				case "upstream-list":
					rows := []any{}
					if registered {
						rows = append(rows, map[string]any{"upstream": map[string]any{"name": "github", "base_url": "ssh://git@github.com/acme"}, "auth": "ok"})
					}
					value = map[string]any{"upstreams": rows}
				case "upstream-add":
					registered = true
					value = map[string]any{"identity": api.UpstreamIdentity{PublicKey: key, Pending: pending}, "upstream": map[string]string{"name": "github"}}
				case "public-identity":
					var request struct {
						Name string `json:"name"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("bad named identity request")
					}
					content := ca
					if request.Name == "upstream-public-key" {
						content = key
					}
					value = map[string]string{"content": content}
				case "version":
					value = map[string]any{"version": "v0.22.6", "schema": 1}
				default:
					t.Errorf("unexpected API action %s", action)
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, CACert: ca, Token: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
			replicas := int32(1)
			kube := fake.NewClientset(preflightNode(), preflightStorage(), readyOberthPod(), readyArgoControllerPod(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}}, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}})
			var output bytes.Buffer
			firstExec := 0
			run := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), " exec ") {
					if !strings.HasSuffix(strings.Join(args, " "), "admin create-first-admin") {
						t.Fatal("extra server exec", args)
					}
					firstExec++
					if bytes.Contains(input, []byte("PRIVATE KEY")) {
						t.Fatal("private enrollment input")
					}
					return json.Marshal(api.FirstAdminResult{Token: "fixture-once", InstallerCertificate: "public fixture certificate", ServerCA: ca})
				}
				return preflightStats(ctx, input, name, args...)
			}
			host := InstallDeps{Output: &output, Admin: remote, ProbeNetworkPolicy: enforcedPolicyFixture, RunCommand: run, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
					return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.22.6"}]`), nil
				}
				return []byte(`{"networkPolicy":{"enabled":true}}`), nil
			}, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
				return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: []byte(ca)}}, "selected", nil
			}}
			cfg := Config{ForgeURL: "ssh://git@github.com/acme", Namespace: DefaultNamespace, ArgoNamespace: DefaultArgoNamespace, ContextName: "selected", UplinkIdentity: "admin@host", ClientAccess: "none", ClientHost: "127.0.0.1", ClientHTTPSPort: port, ClientSSHPort: port, Timeout: time.Second, SecretsFile: filepath.Join(t.TempDir(), "credentials")}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			enrollmentRoot := t.TempDir()
			if err := os.Chmod(enrollmentRoot, 0700); err != nil {
				t.Fatal(err)
			}
			execute := func(ctx context.Context, got Config, deps InstallDeps) error {
				if deps.IsTerminal() {
					t.Fatal("JSON path became interactive")
				}
				onboard := Deps{Admin: remote, KubeClient: kube, RestConfig: &rest.Config{Host: "https://api.example.test"}, Output: deps.Output, Input: strings.NewReader(""), IsTerminal: deps.IsTerminal, ContextName: "selected", RunCommand: run, ClientConfigRoot: func() (string, error) { return enrollmentRoot, nil }, UserHomeDir: func() (string, error) { return enrollmentRoot, nil }, CredentialSink: func(string, string) {}, upstreamResult: deps.upstreamResult, PollInterval: time.Millisecond, ClientHost: cfg.ClientHost, ClientHTTPSPort: port, ClientSSHPort: port}
				tw := newTableWriter(io.Discard, false)
				tw.WriteHeader()
				tw.WriteFooter()
				finishErr := FinishInstall(ctx, got, onboard, tw, &heldCredentials{})
				if finishErr != nil {
					t.Logf("onboarding: %v", finishErr)
				}
				return finishErr
			}
			err = executeJSON(t.Context(), cfg, host, execute)
			var result InstallResult
			if json.Unmarshal(output.Bytes(), &result) != nil {
				t.Fatal("invalid JSON", output.String())
			}
			if result.OK == pending || (err == nil) == pending {
				t.Fatal(result.Checks, err)
			}
			if firstExec != 1 || calls["upstream-add"] != 1 || calls["upstream-list"] == 0 {
				t.Fatal(firstExec, calls)
			}
			if pending {
				if len(result.ManualSteps) == 0 || !strings.Contains(strings.Join(result.ManualSteps, " "), "oberth upstream add") {
					t.Fatal("missing structured commands", result)
				}
			} else if len(result.ManualSteps) != 0 {
				t.Fatal("ready install has manual steps", result.ManualSteps)
			}
			// The same authenticated target resumes without another bootstrap exec.
			output.Reset()
			cfg.existingRelease = true
			if err := executeJSON(t.Context(), cfg, host, execute); err != nil {
				t.Fatal("resume", err, output.String())
			}
			if firstExec != 1 {
				t.Fatal("resume created another first admin")
			}
		})
	}
	for _, change := range []string{"none", "values", "manifest", "force", "resume-upgrade"} {
		t.Run("HelmResume/"+change, func(t *testing.T) {
			chart := t.TempDir()
			if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: oberth\nversion: 0.22.6\nappVersion: 0.22.6\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Namespace: DefaultNamespace, ChartPath: chart, ChartVersion: "0.22.6", ResumeSecretsFile: "/private/recovery", ImageRef: fakeChartImageRef, Upgrade: change == "force" || change == "resume-upgrade"}
			if change == "force" {
				cfg.ResumeSecretsFile = ""
			}
			applies := 0
			previews := 0
			deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				joined := " " + strings.Join(args, " ") + " "
				switch {
				case strings.Contains(joined, " list "):
					return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.22.6","revision":4}]`), nil
				case strings.Contains(joined, " --dry-run=server "):
					previews++
					v := `{"enabled":true}`
					if change == "values" {
						v = `{"enabled":false}`
					}
					manifest := "kind: ConfigMap\n"
					if change == "manifest" {
						manifest = "kind: Service\n"
					}
					return json.Marshal(map[string]any{"config": map[string]any{}, "chart": map[string]any{"values": json.RawMessage(`{"networkPolicy":` + v + `}`)}, "manifest": manifest})
				case strings.Contains(joined, " get values "):
					return []byte(`{"networkPolicy":{"enabled":true}}`), nil
				case strings.Contains(joined, " get manifest "):
					return []byte("kind: ConfigMap\n"), nil
				case strings.Contains(joined, " upgrade "):
					applies++
					return nil, nil
				default:
					return nil, nil
				}
			}}
			result, err := InstallOberth(t.Context(), cfg, deps, OpenBaoResult{}, RekorResult{})
			if err != nil {
				t.Fatal(err)
			}
			if change == "none" || change == "resume-upgrade" {
				if applies != 0 || previews != 1 || !result.AlreadyInstalled {
					t.Fatal("unchanged resume wrote a revision", applies, previews, result)
				}
			} else if applies != 1 {
				t.Fatal("changed or forced install was skipped", change, result)
			}
		})
	}
}

// Keep the generated SSH material behind the same public-key encoding as enrollment.
