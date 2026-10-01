package installer

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// waitForCredentialAcknowledgment was removed — credentials are printed and
// the install continues without a gate. These tests are retired.

// --- Interactive token prompt tests ---

func TestSetupProductionSecretStoreInteractiveTokenPrompt(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	const injectedToken = "s.interactivetoken42"

	responses := configuredProductionStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{
		out: `{"initialized":true,"sealed":false,"storage_type":"file"}`,
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	var buf bytes.Buffer

	ca := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-public"},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openbao-0", Namespace: DefaultOpenBaoNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "openbao"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	deps := Deps{
		Output:       &buf,
		Input:        strings.NewReader(""),
		KubeClient:   fake.NewClientset(ca, pod),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		RunCommand:   runner.run,
		ContextName:  "test-ctx",
		PollInterval: time.Millisecond,
		IsTerminal:   func() bool { return true },
		RunInteractive: func(context.Context, string, ...string) error {
			return nil
		},
		ReadPassword: func() ([]byte, error) {
			return []byte(injectedToken), nil
		},
	}
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	configured, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err != nil {
		t.Fatal(err)
	}
	if !configured.TrustedTransitVerified {
		t.Fatalf("interactive token prompt did not lead to verified Transit: %+v", configured)
	}

	output := buf.String()
	// Assert the prompt was shown
	if !strings.Contains(output, "Root token (echo disabled):") {
		t.Fatalf("output missing echo-disabled prompt:\n%s", output)
	}
	// Assert the token string never appears in the output (security invariant)
	if strings.Contains(output, injectedToken) {
		t.Fatalf("token value must never appear in output:\n%s", output)
	}
	// Assert no inline BAO_TOKEN='...' template in output
	if strings.Contains(output, "BAO_TOKEN='") {
		t.Fatalf("output must not contain inline BAO_TOKEN assignment template:\n%s", output)
	}

	// Verify the injected token was actually used for authenticated calls
	authenticated := 0
	for _, call := range runner.calls {
		if call.authenticated {
			authenticated++
			if !strings.HasPrefix(call.stdin, injectedToken+"\n") {
				t.Fatalf("authenticated call %q must use the injected token, got stdin %q", call.command, call.stdin)
			}
		}
	}
	if authenticated == 0 {
		t.Fatal("expected authenticated configuration calls with the injected token")
	}
}

func TestSetupProductionSecretStoreNonInteractiveTokenPromptNotShown(t *testing.T) {
	// Verify that the existing non-interactive test behavior is unchanged
	// and that no inline BAO_TOKEN='...' template appears in the output.
	t.Setenv(baoTokenEnvVar, "")
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {out: `{"initialized":true,"sealed":false,"storage_type":"file"}`},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openbao-0", Namespace: DefaultOpenBaoNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "openbao"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	ca := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-public"},
		Data:       map[string]string{"ca.crt": "fake-ca"},
	}
	var buf bytes.Buffer
	deps := Deps{
		Output:       &buf,
		KubeClient:   fake.NewClientset(ca, pod),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		RunCommand:   runner.run,
		ContextName:  "test-ctx",
		PollInterval: time.Millisecond,
		// Non-interactive: no IsTerminal, no ReadPassword
	}
	cfg := Config{InstallSecretStore: true}
	_ = cfg.Validate()

	_, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err == nil || !strings.Contains(err.Error(), baoTokenEnvVar) {
		t.Fatalf("non-interactive missing-token error = %v", err)
	}
	output := buf.String()
	if strings.Contains(output, "Root token (echo disabled):") {
		t.Fatalf("non-interactive must not show echo-disabled prompt:\n%s", output)
	}
	if strings.Contains(output, "BAO_TOKEN='") {
		t.Fatalf("output must not contain inline BAO_TOKEN assignment template:\n%s", output)
	}
}

// --- Org name validation tests (issue #411) ---

func TestValidateOrgNameRejectsHCLInjection(t *testing.T) {
	// An org name containing HCL injection characters must be rejected
	// before it reaches writeUpstreamOrgRules, where it would be
	// interpolated unescaped into a Vault policy path string.
	malicious := `evil" { capabilities = ["sudo"] } path "secret/*`
	err := ValidateOrgName(malicious)
	if err == nil {
		t.Fatal("expected rejection of malicious org name with HCL injection characters")
	}
	if !strings.Contains(err.Error(), "characters outside") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateOrgNameRejectsControlChars(t *testing.T) {
	for _, name := range []string{"org\x00evil", "org\nevil", "org\tevil"} {
		if err := ValidateOrgName(name); err == nil {
			t.Errorf("ValidateOrgName(%q) = nil; want error for control character", name)
		}
	}
}

func TestValidateOrgNameAcceptsValid(t *testing.T) {
	for _, name := range []string{"oberthci", "skip-ops", "my.org", "org_name", "MixedCase123", "a"} {
		if err := ValidateOrgName(name); err != nil {
			t.Errorf("ValidateOrgName(%q) = %v; want nil", name, err)
		}
	}
}

func TestValidateOrgNameRejectsEmpty(t *testing.T) {
	if err := ValidateOrgName(""); err == nil {
		t.Fatal("ValidateOrgName(\"\") = nil; want error for empty string")
	}
}

func TestValidateOrgNameRejectsSlash(t *testing.T) {
	// A slash would allow path traversal in the Vault policy.
	if err := ValidateOrgName("org/evil"); err == nil {
		t.Fatal("ValidateOrgName(\"org/evil\") = nil; want error for slash")
	}
}

// --- Repo name validation tests (issue #416) ---

func TestValidateRepoNameRejectsHCLInjection(t *testing.T) {
	// A repo name containing HCL injection characters must be rejected
	// before it reaches PerRepoPolicy, where it would be interpolated
	// unescaped into a Vault policy path string.
	malicious := `evil" { capabilities = ["sudo"] } path "secret/*`
	err := ValidateRepoName(malicious)
	if err == nil {
		t.Fatal("expected rejection of malicious repo name with HCL injection characters")
	}
	if !strings.Contains(err.Error(), "characters outside") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRepoNameRejectsControlChars(t *testing.T) {
	for _, name := range []string{"repo\x00evil", "repo\nevil", "repo\tevil"} {
		if err := ValidateRepoName(name); err == nil {
			t.Errorf("ValidateRepoName(%q) = nil; want error for control character", name)
		}
	}
}

func TestValidateRepoNameRejectsBackslash(t *testing.T) {
	if err := ValidateRepoName(`repo\nevil`); err == nil {
		t.Fatal(`ValidateRepoName("repo\nevil") = nil; want error for backslash`)
	}
}

func TestValidateRepoNameRejectsBraces(t *testing.T) {
	if err := ValidateRepoName("repo{evil}"); err == nil {
		t.Fatal("ValidateRepoName(\"repo{evil}\") = nil; want error for braces")
	}
}

func TestValidateRepoNameAcceptsValid(t *testing.T) {
	for _, name := range []string{"oberth", "acme-operator", "my.repo", "repo_name", "MixedCase123", "a"} {
		if err := ValidateRepoName(name); err != nil {
			t.Errorf("ValidateRepoName(%q) = %v; want nil", name, err)
		}
	}
}

func TestValidateRepoNameRejectsEmpty(t *testing.T) {
	if err := ValidateRepoName(""); err == nil {
		t.Fatal("ValidateRepoName(\"\") = nil; want error for empty string")
	}
}

func TestValidateRepoNameRejectsSlash(t *testing.T) {
	// A slash would allow path traversal in the Vault policy.
	if err := ValidateRepoName("repo/evil"); err == nil {
		t.Fatal("ValidateRepoName(\"repo/evil\") = nil; want error for slash")
	}
}

// --- Credentialed secret path validation tests (issue #414) ---

func TestCredentialedPolicyPathsRejectsHCLInjection(t *testing.T) {
	// A path containing HCL metacharacters must be rejected before it
	// reaches OberthCredentialedPolicyWithGrants, where it would be
	// interpolated unescaped into a Vault policy path string.
	hostile := `oberth/data/x" { capabilities = ["sudo"] } path "y`
	_, err := credentialedPolicyPaths("oberth", []string{hostile})
	if err == nil {
		t.Fatal("expected rejection of hostile path with HCL injection characters")
	}
	if !strings.Contains(err.Error(), "characters outside") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCredentialedPolicyPathsRejectsBackslash(t *testing.T) {
	_, err := credentialedPolicyPaths("oberth", []string{`oberth/data/release/path\nevil`})
	if err == nil {
		t.Fatal("expected rejection of path containing backslash")
	}
}

func TestCredentialedPolicyPathsRejectsControlChars(t *testing.T) {
	for _, path := range []string{
		"oberth/data/release/x\x00y",
		"oberth/data/release/x\ny",
		"oberth/data/release/x\ty",
	} {
		if _, err := credentialedPolicyPaths("oberth", []string{path}); err == nil {
			t.Errorf("credentialedPolicyPaths(%q) = nil; want error for control character", path)
		}
	}
}

func TestCredentialedPolicyPathsAcceptsValid(t *testing.T) {
	valid := []string{
		"oberth/data/release/cosign-secret",
		"oberth/upstream/oberthci/oberth/secret_name",
		"oberth/data/release/my.dotted.path",
	}
	out, err := credentialedPolicyPaths("oberth", valid)
	if err != nil {
		t.Fatalf("credentialedPolicyPaths(valid) = %v; want nil", err)
	}
	if len(out) != len(valid) {
		t.Fatalf("got %d paths, want %d", len(out), len(valid))
	}
}

// productionOpenBaoResult is defined in installer_test.go (same package).
