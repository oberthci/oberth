package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("settings file is no longer JSON: %v\n%s", err, body)
	}
	return document
}

// The whole file belongs to the operator. Naming the CA must leave every other
// setting, and every other variable in env, exactly where it was.
func TestCATrustIsMergedIntoTheSettingsThatAreAlreadyThere(t *testing.T) {
	t.Parallel()
	existing := []byte(`{"model":"opus","env":{"FOO":"bar"},"permissions":{"allow":["Bash"]}}`)
	body, changed, err := mergeNodeExtraCACerts(existing, "/home/me/.config/oberth/ca.crt")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document["model"] != "opus" {
		t.Errorf("model was lost: %v", document["model"])
	}
	if document["permissions"] == nil {
		t.Error("permissions were lost")
	}
	env, _ := document["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Errorf("an existing environment variable was lost: %v", env)
	}
	if env[nodeExtraCACerts] != "/home/me/.config/oberth/ca.crt" {
		t.Errorf("the CA was not named: %v", env)
	}
}

// A settings file with no env object at all is the common case on a machine
// that has never needed one.
func TestCATrustCreatesTheEnvObjectWhenThereIsNone(t *testing.T) {
	t.Parallel()
	body, changed, err := mergeNodeExtraCACerts([]byte(`{"model":"opus"}`), "/ca.crt")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(body), nodeExtraCACerts) {
		t.Fatalf("env object not created:\n%s", body)
	}
}

// Re-running an install must not rewrite a file that already says the right
// thing.
func TestCATrustAlreadyNamedChangesNothing(t *testing.T) {
	t.Parallel()
	existing := []byte(`{"env":{"NODE_EXTRA_CA_CERTS":"/ca.crt"}}`)
	body, changed, err := mergeNodeExtraCACerts(existing, "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	if changed || body != nil {
		t.Fatalf("an unchanged file was rewritten: changed=%v body=%s", changed, body)
	}
}

// A different value is another deployment's CA, or something the operator set
// deliberately. Ours is not more likely to be the right one, so this stops.
func TestCATrustDoesNotClobberADifferentValue(t *testing.T) {
	t.Parallel()
	existing := []byte(`{"env":{"NODE_EXTRA_CA_CERTS":"/somewhere/else.crt"}}`)
	_, changed, err := mergeNodeExtraCACerts(existing, "/ca.crt")
	if !errors.Is(err, errCATrustAlreadySet) {
		t.Fatalf("err=%v, want errCATrustAlreadySet", err)
	}
	if changed {
		t.Error("reported a change while refusing to make one")
	}
	if !strings.Contains(err.Error(), "/somewhere/else.crt") {
		t.Errorf("the message does not say what is already there: %v", err)
	}
}

// A file that will not parse is someone's configuration with a typo in it.
// Replacing it destroys everything else they have set.
func TestCATrustRefusesToParseFileAndLeavesItAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{\"model\": \"opus\",}\n")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := trustCAInClaudeCode("/ca.crt"); err == nil {
		t.Fatal("a malformed settings file was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(broken) {
		t.Fatalf("the operator's file was destroyed:\n%s", after)
	}
}

// The end-to-end write, through the same home directory Claude Code reads.
func TestCATrustWritesTheSettingsFileClaudeCodeReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	caPath := filepath.Join(home, ".config", "oberth", "ca.crt")

	outcome := claudeCATrust(caPath, true)
	if outcome.status != "✓ trusted" {
		t.Fatalf("status %q detail %q", outcome.status, outcome.detail)
	}
	document := readSettings(t, filepath.Join(home, ".claude", "settings.json"))
	env, _ := document["env"].(map[string]any)
	if env[nodeExtraCACerts] != caPath {
		t.Fatalf("the CA was not written: %v", document)
	}
}

// Silence is the failure this replaces: a client configured against a server
// it cannot verify, and nothing on the terminal saying so. A settings file
// that cannot be merged still has to say what to set by hand.
func TestAClientThatCannotBeConfiguredSaysWhatToDoByHand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome := claudeCATrust(filepath.Join(home, "ca.crt"), true)
	if outcome.status != "⚠ manual" {
		t.Fatalf("status %q, want manual", outcome.status)
	}
	if outcome.note == "" {
		t.Fatal("nothing was said about the CA")
	}
}

// The rule this project keeps relearning: a write that returned no error is
// not a client that connects. An unverified handshake must never print as
// success, however well the file was written.
func TestAWriteThatCouldNotBeVerifiedIsNotSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outcome := claudeCATrust(filepath.Join(home, "ca.crt"), false)
	if outcome.status != "⚠ unverified" {
		t.Fatalf("status %q, want unverified", outcome.status)
	}
}

// The verification is a real handshake, so a server this CA really signed is
// the only thing that passes it.
func TestServerTrustIsAnActualHandshake(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	authority := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	})

	if err := verifyServerTrust(context.Background(), server.URL, authority); err != nil {
		t.Fatalf("the server's own certificate did not verify it: %v", err)
	}

	if err := verifyServerTrust(context.Background(), server.URL, unrelatedCA(t)); err == nil {
		t.Fatal("a server this CA never signed was reported as verified")
	}
}

// A CA file that carries no certificate is the failure that produced an
// "unable to verify the first certificate" hours later.
func TestServerTrustRejectsAnEmptyPool(t *testing.T) {
	t.Parallel()
	if err := verifyServerTrust(context.Background(), "https://localhost:30443", []byte("not a certificate")); err == nil {
		t.Fatal("an empty certificate pool was accepted")
	}
}

// unrelatedCA is a certificate that signed nothing in this test: the pool a
// client would have if it were pointed at another deployment's ca.crt.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "somebody else"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
