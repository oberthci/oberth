package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestAnswersStrictRoundTrip(t *testing.T) {
	bootstrapCredentialInventory(t)
	parallelism, namespaceParallelism := 7, 3
	cfg := Config{ArgoParallelism: &parallelism, ArgoNamespaceParallelism: &namespaceParallelism, ContextName: "selected", InstallSecretStore: true, ClientAccess: "none", AlsoUpstreams: []string{"codeberg.org/acme"}, TLSExtraIPs: []string{"::1"}, SecretsFile: "/secure/install", AdminSSHPublicKeyPath: "-"}
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := WriteAnswers(path, cfg); err != nil {
		t.Fatal(err)
	}
	actual, err := ReadAnswers(path)
	if err != nil || !reflect.DeepEqual(cfg, actual) {
		t.Fatal("answers did not round trip")
	}
	if err := WriteAnswers(path, cfg); err == nil {
		t.Fatal("overwrote answers")
	}
	for _, raw := range []string{`version: 1
config:
  Unknown: hidden`, `version: 1
version: 1
config: {}`, `version: 2
config: {}`, `version: 1
config:
  Release: {Unknown: hidden}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAnswers(path); err == nil || strings.Contains(err.Error(), "hidden") {
			t.Fatal("invalid answers accepted or echoed")
		}
	}
	if err := os.WriteFile(path, []byte("version: 1\nconfig:\n  ContextName: selected\n  InstallSecretStore: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err = ReadAnswers(path)
	if err != nil || actual.ContextName != "selected" || !actual.InstallSecretStore {
		t.Fatal("YAML answers not read")
	}
}

func TestVerificationJSONRedactsDependencyErrors(t *testing.T) {
	var output bytes.Buffer
	marker := "sensitive-dependency-diagnostic"
	deps := InstallDeps{ProbeNetworkPolicy: enforcedPolicyFixture, Output: &output, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
		return nil, nil, "", errors.New(marker)
	}}
	if err := PrintVerification(context.Background(), Config{}, deps, true); err == nil {
		t.Fatal("failed verification succeeded")
	}
	var result InstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Checks) != 1 || result.Checks[0].Code != "kubeconfig" || result.Checks[0].Remedy == "" || strings.Contains(output.String(), marker) {
		t.Fatal("unsafe or incomplete verification response")
	}
}

func TestAdminKeyStdinRejectsPrivateBeforeSideEffects(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(body)
	defer clear(privatePEM)
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", private.Seed())
	if err != nil {
		t.Fatal("cannot prepare encrypted private-key fixture")
	}
	encryptedPEM := pem.EncodeToMemory(encrypted)
	defer clear(encryptedPEM)
	for _, input := range [][]byte{privatePEM, append(ssh.MarshalAuthorizedKey(key), privatePEM...), append(ssh.MarshalAuthorizedKey(key), encryptedPEM...)} {
		called := false
		err = Execute(context.Background(), Config{AdminSSHPublicKeyPath: "-"}, InstallDeps{Input: bytes.NewReader(input), LookPath: func(string) (string, error) { called = true; return "", nil }})
		if err == nil || called {
			t.Fatal("private key reached install side effects")
		}
	}
}

func TestSecretsAgeRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("age"); err != nil {
		t.Skip("age is a test prerequisite")
	}
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen is a test prerequisite")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(directory, "identity")
	command := exec.Command("age-keygen", "-o", identity)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if command.Run() != nil {
		t.Fatal("identity creation failed")
	}
	recipient, err := exec.Command("age-keygen", "-y", identity).Output()
	if err != nil {
		t.Fatal("recipient read failed")
	}
	path := filepath.Join(directory, "secrets.age")
	secrets, err := createSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.enableEncryption(context.Background(), strings.TrimSpace(string(recipient))); err != nil {
		t.Fatal(err)
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	credential := fmt.Sprintf("%x", value)
	secrets.add("Unseal key", credential)
	secrets.add("Bearer token", credential+"-second")
	var output bytes.Buffer
	secrets.finish(&output)
	if secrets.err != nil {
		t.Fatal("secret encryption failed")
	}
	if strings.Contains(output.String(), credential) {
		t.Fatal("secret appeared in output")
	}
	ciphertext, err := os.ReadFile(path)
	if err != nil || bytes.Contains(ciphertext, []byte(credential)) {
		t.Fatal("plaintext reached disk")
	}
	plaintext, err := exec.Command("age", "-d", "-i", identity, path).Output()
	defer clear(plaintext)
	if err != nil || !bytes.Contains(plaintext, []byte(credential+"-second")) {
		t.Fatal("encrypted credentials did not round trip")
	}
	// Execute the user-facing handoff against the real ciphertext. The token
	// must reach only the selected store, never shell output or command text.
	destination := filepath.Join(directory, "stored-token")
	handoff := storeFromSecretsFile("cat > "+shellQuoteWord(destination), path, true)
	handoff = strings.ReplaceAll(handoff, "/path/to/age-identity", shellQuoteWord(identity))
	if strings.Contains(handoff, credential) {
		t.Fatal("credential in handoff argv")
	}
	var captured bytes.Buffer
	command = exec.Command("sh", "-c", handoff)
	command.Stdout, command.Stderr = &captured, &captured
	if command.Run() != nil || bytes.Contains(captured.Bytes(), []byte(credential)) {
		t.Fatal("encrypted credential handoff failed or leaked output")
	}
	stored, err := os.ReadFile(destination)
	defer clear(stored)
	if err != nil || string(stored) != credential+"-second\n" {
		t.Fatal("encrypted credential did not reach its store")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("incorrect encrypted file permissions")
	}
}

func TestFirstAdminBeforeForge(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeOberthHost{t: t, uplinkOutput: "Uplink token for admin@workstation (shown once):\n" + testOnceCredential + "\n"}
	var output bytes.Buffer
	deps := onboardingDeps(t, host, &output, strings.NewReader(""), false)
	identity := ""
	deps.UplinkCreated = func(value string) { identity = value }
	cfg := Config{AdminSSHPublicKeyPath: "-", SSHPublicKeyPath: "-", adminSSHPublicKey: ssh.MarshalAuthorizedKey(key), UplinkIdentity: "admin@workstation", ClientAccess: "none"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tw := newTableWriter(&output, false)
	tw.WriteHeader()
	tw.WriteFooter()
	delivered := false
	creds := heldCredentials{atOnce: func(label, value string) {
		if label == "Bearer token" && value == testOnceCredential {
			delivered = true
		}
	}}
	if err := FinishInstall(context.Background(), cfg, deps, tw, &creds); !errors.Is(err, ErrOnboardingPartial) {
		t.Fatal("expected pending forge")
	}
	if !delivered || identity != cfg.UplinkIdentity || !strings.Contains(strings.Join(host.uplinkArgv, " "), "uplink add --admin - admin@workstation") || strings.Contains(output.String(), testOnceCredential) {
		t.Fatal("first admin flow failed or leaked a credential")
	}
}

func TestVerificationPublicEndpointsAndCAPin(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	kube := fake.NewClientset(preflightNode(), preflightStorage(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}}, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}})
	forgeAuthorized := true
	readinessJSON := `{"status":"not ready","code":"no_upstream"}`
	argoReads, argoResourceReads := 0, 0
	deps := InstallDeps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		command := " " + strings.Join(args, " ") + " "
		if strings.Contains(command, " list ") {
			if strings.Contains(command, "-n "+DefaultArgoNamespace+" ") {
				return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
			}
			return []byte(`[{"name":"oberth"}]`), nil
		}
		if strings.Contains(command, " get values argo-workflows ") {
			if strings.Contains(command, " --all ") {
				argoReads++
				return []byte(`{"controller":{"parallelism":17,"namespaceParallelism":5}}`), nil
			}
			argoResourceReads++
			return []byte(`{"executor":{"resources":{"requests":{"cpu":"75m"}}}}`), nil
		}
		return []byte(`{"networkPolicy":{"enabled":true}}`), nil
	}, ProbeNetworkPolicy: enforcedPolicyFixture, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
		return kube, &rest.Config{Host: "https://127.0.0.1:6443", TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, "selected", nil
	}, RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		command := strings.Join(args, " ")
		if strings.Contains(command, "stats/summary") {
			return preflightStats(context.Background(), nil, "kubectl")
		}
		if strings.Contains(command, "upstream list --check") {
			if !forgeAuthorized {
				return []byte("NAME   KIND URL                              AUTH\nforge  ssh  ssh://git@forge.example/acme      refused\n"), nil
			}
			return []byte("NAME   KIND URL                              AUTH\nforge  ssh  ssh://git@forge.example/acme      ok\n"), nil
		}
		if strings.Contains(command, "admin read-file") {
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), nil
		}
		if strings.Contains(command, "admin readiness") {
			return []byte(readinessJSON), nil
		}
		return nil, errors.New("unexpected command")
	}}
	cfg := Config{ClientHost: "127.0.0.1", ClientHTTPSPort: port, ClientSSHPort: port}
	result := VerifyInstall(context.Background(), cfg, deps)
	digest := sha256.Sum256(server.Certificate().Raw)
	if !result.OK || result.CAPin != hex.EncodeToString(digest[:]) || result.ServerURL != server.URL || result.MCPURL != server.URL+"/mcp" || result.Context != "selected" {
		t.Fatalf("public verification failed: checks=%v", result.Checks)
	}

	if result.Review == nil || result.Review.ArgoParallelism != "17 (preserved)" || result.Review.ArgoNamespaceParallelism != "5 (preserved)" || argoReads != 1 || argoResourceReads != 2 {
		t.Fatal("verify omitted current native Argo values", result.Review, argoReads)
	}
	assertCurrentResources := func(review *InstallReview) {
		t.Helper()
		if review == nil || review.ArgoExecutorResources == nil || !review.ArgoExecutorResources.OperatorSet {
			t.Fatal("current operator resources omitted", review)
		}
		raw, err := json.Marshal(review.ArgoExecutorResources.Resources)
		if err != nil || string(raw) != `{"requests":{"cpu":"75m"}}` {
			t.Fatal("stale executor resource snapshot", string(raw), err)
		}
	}
	assertCurrentResources(result.Review)
	// A fresh apply carries snapshots in which the releases were absent.
	// Successful JSON output must read the now-installed releases again.
	var output bytes.Buffer
	postHost := deps
	postHost.Output = &output
	postCfg := cfg
	postCfg.argoValuesRead, postCfg.existingValuesRead, postCfg.argoReleaseRead = true, true, true
	postCfg.argoExecutorResources = &ArgoExecutorResourcesReview{OperatorSet: true, Resources: map[string]any{"requests": map[string]any{"cpu": "1m"}}}
	if err := executeJSON(t.Context(), postCfg, postHost, func(context.Context, Config, InstallDeps) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var installed InstallResult
	if err := json.Unmarshal(output.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	if !installed.OK || installed.Review == nil || installed.Review.ArgoParallelism != "17 (preserved)" || installed.Review.ArgoNamespaceParallelism != "5 (preserved)" || argoReads != 2 || argoResourceReads != 4 {
		t.Fatal("successful JSON output reused preflight defaults", installed.Review, argoReads)
	}
	assertCurrentResources(installed.Review)
	service, err := kube.CoreV1().Services(DefaultNamespace).Get(t.Context(), "oberth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.Annotations = map[string]string{clientHostAnnotation: "127.0.0.1", clientSSHPortAnnotation: port, clientHTTPSPortAnnotation: port}
	if _, err := kube.CoreV1().Services(DefaultNamespace).Update(t.Context(), service, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if persisted := VerifyInstall(t.Context(), Config{}, deps); !persisted.OK || persisted.ServerURL != server.URL || persisted.GitURL != result.GitURL {
		t.Fatalf("verify lost persisted endpoints: %+v", persisted.Checks)
	}
	deployment, err := kube.AppsV1().Deployments(DefaultNamespace).Get(t.Context(), "oberth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 0
	if _, err := kube.AppsV1().Deployments(DefaultNamespace).UpdateStatus(t.Context(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"no_upstream", "database_error", "ssh_identity", "unavailable"} {
		readinessJSON = `{"status":"not ready","code":"` + reason + `"}`
		if reason == "unavailable" {
			readinessJSON = `{"status":"not ready","code":"private dependency details"}`
		}
		// Invalid DNS/ports would fail if verify ignored deployment readiness.
		blocked := VerifyInstall(t.Context(), Config{ClientHost: "override.invalid", ClientSSHPort: "32022", ClientHTTPSPort: "32443"}, deps)
		if blocked.ServerURL != "https://override.invalid:32443" || blocked.GitURL != "ssh://git@override.invalid:32022" {
			t.Fatal("explicit flags did not override stored endpoints")
		}
		seen := 0
		for _, check := range blocked.Checks {
			switch check.Code {
			case "deployment_ready", "client_bootstrap_endpoint", "ssh_reachable":
				seen++
				if check.OK || check.Reason != reason {
					t.Fatalf("lost readiness cause: %+v", check)
				}
				if check.Code != "deployment_ready" && check.Remedy != "blocked by: deployment not ready ("+reason+")" {
					t.Fatalf("misleading dependent remedy: %+v", check)
				}
			}
		}
		if seen != 3 {
			t.Fatal("missing readiness or blocked connectivity check")
		}
	}
	deployment.Status.ReadyReplicas = 1
	if _, err := kube.AppsV1().Deployments(DefaultNamespace).UpdateStatus(t.Context(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	forgeAuthorized = false
	if VerifyInstall(context.Background(), cfg, deps).OK {
		t.Fatal("forge with a rejected deploy key passed verification")
	}
	forgeAuthorized = true
	if err := kube.CoreV1().Services(DefaultNamespace).Delete(context.Background(), "oberth", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if VerifyInstall(context.Background(), cfg, deps).OK {
		t.Fatal("missing Service passed verification")
	}
	if _, err := kube.CoreV1().Services(DefaultNamespace).Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg.ClientHost = "invalid.example.test"
	result = VerifyInstall(context.Background(), cfg, deps)
	if result.OK {
		t.Fatal("unreachable advertised address accepted")
	}
}

func TestUpstreamRecoverySavedBeforeImport(t *testing.T) {
	saved := false
	var recovery string
	creds := heldCredentials{atOnce: func(label, value string) {
		if label == "Upstream private key" {
			recovery = value
			saved = true
		}
	}}
	calls := 0
	deps := Deps{RunCommand: func(_ context.Context, input []byte, _ string, args ...string) ([]byte, error) {
		calls++
		if strings.Contains(strings.Join(args, " "), "admin read-file") {
			return nil, errors.New("not present")
		}
		if !saved || recovery != string(input) {
			t.Fatal("new key used before recovery was saved")
		}
		if _, err := ssh.ParsePrivateKey(input); err != nil {
			t.Fatal("invalid generated key")
		}
		if strings.Contains(strings.Join(args, " "), recovery) {
			t.Fatal("key on argv")
		}
		return nil, nil
	}}
	provided, err := prepareUpstreamRecovery(context.Background(), Config{SecretsFile: "/secure/recovery", ForgeURL: "ssh://git@forge.example/acme"}, deps, &creds, false)
	if err != nil || !provided || calls != 2 {
		t.Fatal("recovery generation failed")
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return ssh.MarshalAuthorizedKey(key), nil
	}
	saved = false
	provided, err = prepareUpstreamRecovery(context.Background(), Config{SecretsFile: "/secure/recovery"}, deps, &creds, false)
	if err != nil || provided || saved {
		t.Fatal("existing upstream key was replaced")
	}
}
