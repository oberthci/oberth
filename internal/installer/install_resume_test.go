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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	testJSONFirstOnboarding(t)
	testJSONDependencyResume(t)
	testJSONOberthResume(t)
}
func testJSONFirstOnboarding(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("FirstRunPending=%t", pending), func(t *testing.T) { testJSONFirstOnboardingCase(t, pending, "none") })
	}
}

func TestFirstAdminRerunRefusalPreservesPartialResult(t *testing.T) {
	testJSONFirstOnboardingCase(t, false, "refused")
}

func TestFirstAdminInterruptedInstallResumes(t *testing.T) {
	for _, interruption := range []string{"before-exec", "before-exec-explicit", "lost-response"} {
		t.Run(interruption, func(t *testing.T) { testJSONFirstOnboardingCase(t, false, interruption) })
	}
}

func testJSONFirstOnboardingCase(t *testing.T, pending bool, interruption string) {
	public, private := newEd25519HostKey(t)
	bootstrapCredentialInventory(t, private)
	defer clear(private)
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
	registered := false
	var authenticated atomic.Bool
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
			if !authenticated.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			value = map[string]any{"version": "v0.22.6", "schema": 1}
		default:
			t.Errorf("unexpected API action %s", action)
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	bootstrapPrivateKey(t, server.TLS.Certificates[0].PrivateKey)
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
	firstCSR := ""
	run := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " exec ") {
			if !strings.HasSuffix(strings.Join(args, " "), "admin create-first-admin") {
				t.Fatal("extra server exec", args)
			}
			firstExec++
			var enrollment api.FirstAdminRequest
			if json.Unmarshal(input, &enrollment) != nil {
				t.Fatal("invalid exec request")
			}
			if firstCSR == "" {
				firstCSR = enrollment.InstallerCSR
			} else if enrollment.InstallerCSR != firstCSR {
				t.Fatal("resumed install replaced the persisted enrollment key")
			}
			if interruption == "refused" {
				return []byte(`{"token":"never-print-this-stdout"}`), &exec.ExitError{Stderr: []byte("oberth: an active unscoped admin exists; retry with the original installer key or use authenticated administrative APIs")}
			}
			if interruption == "lost-response" && firstExec == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			authenticated.Store(true)
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
	if interruption == "before-exec-explicit" {
		path := filepath.Join(t.TempDir(), "operator.pub")
		if err := os.WriteFile(path, []byte(key), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.AdminSSHPublicKeyPath = path
		cfg.SSHPublicKeyPath = path
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	enrollmentRoot := t.TempDir()
	defer bootstrapPrivateFiles(t, enrollmentRoot)
	defer bootstrapInstallerEnrollments(t, enrollmentRoot)
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

	cfg.existingRelease = strings.HasPrefix(interruption, "before-exec")
	if interruption == "lost-response" {
		if err := executeJSON(t.Context(), cfg, host, execute); err == nil {
			t.Fatal("interrupted exec response was accepted")
		}
		cfg.existingRelease = true
		output.Reset()
	}
	err = executeJSON(t.Context(), cfg, host, execute)
	var result InstallResult
	if json.Unmarshal(output.Bytes(), &result) != nil {
		t.Fatal("invalid JSON", output.String())
	}
	if interruption == "refused" {
		if err == nil || result.OK || len(result.ManualSteps) == 0 || result.Context != "selected" || result.Review == nil {
			t.Fatal("partial install result lost", result, err)
		}
		found := false
		for _, check := range result.Checks {
			if check.Code == "first_admin" && strings.Contains(check.Reason, "an active unscoped admin exists") {
				found = true
			}
		}
		if !found || strings.Contains(output.String(), "never-print-this-stdout") {
			t.Fatal("server refusal lost or credential stdout exposed", result.Checks)
		}
		return
	}
	if result.OK == pending || (err == nil) == pending {
		t.Fatal(result.Checks, err)
	}
	wantExec := 1
	if interruption == "lost-response" {
		wantExec = 2
	}
	if firstExec != wantExec || calls["upstream-add"] != 1 || calls["upstream-list"] == 0 {
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
	if firstExec != wantExec {
		t.Fatal("resume created another first admin")
	}
}

func testJSONDependencyResume(t *testing.T) {
	for _, name := range []string{"openbao", "argo-workflows"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("DependencyResume/%s/changed=%t", name, changed), func(t *testing.T) {
				version := DefaultOpenBaoChartVersion
				if name == "argo-workflows" {
					version = DefaultArgoChartVersion
				}
				applies, previews := 0, 0
				deps := Deps{Output: io.Discard, KubeClient: argoSecurityTestClient(), RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					switch args[0] {
					case "list":
						return json.Marshal([]helmRelease{{Name: name, Status: "deployed", Chart: name + "-" + version}})
					case "get":
						if args[1] == "manifest" {
							return []byte("kind: Deployment"), nil
						}
						return []byte(`{}`), nil
					case "upgrade":
						if slices.Contains(args, "--dry-run=server") {
							previews++
							manifest := "kind: Deployment"
							if changed {
								manifest = "kind: StatefulSet"
							}
							return json.Marshal(map[string]any{"config": map[string]any{}, "manifest": manifest})
						}
						applies++
					}
					return nil, nil
				}}
				cfg := Config{Upgrade: true, ResumeSecretsFile: "/private/recovery", InstallSecretStoreDev: true}
				var already bool
				if name == "openbao" {
					result, err := InstallOpenBao(t.Context(), cfg, deps)
					if err != nil {
						t.Fatal(err)
					}
					already = result.AlreadyInstalled
				} else {
					result, err := InstallArgoWorkflows(t.Context(), cfg, deps)
					if err != nil {
						t.Fatal(err)
					}
					already = result.AlreadyInstalled
				}
				if previews != 1 || already == changed || applies != map[bool]int{false: 0, true: 1}[changed] {
					t.Fatal("resume revision", previews, applies, already)
				}
			})
		}
	}

}

func testJSONOberthResume(t *testing.T) {
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
			deps := Deps{Output: io.Discard, KubeClient: argoSecurityTestClient(), RunHelm: func(_ context.Context, args []string) ([]byte, error) {
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
