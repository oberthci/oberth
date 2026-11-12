package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// This exercises real age subprocesses, including /dev/fd/3 in the production
// decryptor. Only the Kubernetes/OpenBao transport is simulated.
func TestInstallAgeRecoveryUnsealRoundTrip(t *testing.T) {
	path, identity, rootToken, key := ageInstallRecoveryFixture(t)
	defer clear(rootToken)
	defer clear(key)
	cipher, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read encrypted snapshot")
	}
	plain, err := decryptRecovery(t.Context(), cipher, identity)
	defer clear(plain)
	if err != nil || !bytes.Contains(plain, append([]byte("OPENBAO_ROOT_TOKEN="), rootToken...)) {
		t.Fatal("install recovery snapshot did not decrypt through the inherited descriptor")
	}

	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "unseal", true: "unseal_failure"}[fail], func(t *testing.T) {
			sealed, writes := true, 0
			var output bytes.Buffer
			kube := fake.NewClientset(runningOpenBaoPod(), runningOpenBaoStatefulSet())
			ca := newTestServerCA(t, []string{"api.example.test"})
			deps := InstallDeps{
				Output:   &output,
				LookPath: func(name string) (string, error) { return name, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
					argv := strings.Join(args, " ")
					if name != "kubectl" || !strings.Contains(argv, "--context chosen") || strings.Contains(argv, string(key)) || strings.Contains(argv, string(rootToken)) {
						t.Fatal("unseal lost its target or disclosed a credential in argv")
					}
					if strings.Contains(argv, "sys/unseal") {
						writes++
						var request struct {
							Key string `json:"key"`
						}
						if json.Unmarshal(input, &request) != nil || request.Key != string(key) {
							t.Fatal("unseal did not receive the installed key over stdin")
						}
						if fail {
							return bytes.Clone(key), errors.New(string(rootToken))
						}
						sealed = false
						return bytes.Clone(key), nil
					}
					return json.Marshal(SealStatus{Initialized: true, Sealed: sealed})
				},
			}
			status, callErr := ExecuteUnseal(t.Context(), UnsealConfig{ContextName: "chosen", SecretsFile: path, AgeIdentity: identity}, deps)
			if (callErr != nil) != fail || writes != 1 || (!fail && status.Sealed) {
				t.Fatal("unexpected encrypted-file unseal result")
			}
			if err := PrintSealStatus(&output, status, true); err != nil {
				t.Fatal("cannot render public seal status")
			}
			if callErr != nil {
				output.WriteString(callErr.Error())
			}
			if bytes.Contains(output.Bytes(), key) || bytes.Contains(output.Bytes(), rootToken) {
				t.Fatal("unseal disclosed recovery material")
			}
		})
	}

	wrongIdentity, _ := realAgeIdentity(t)
	for _, test := range []struct {
		name     string
		cipher   []byte
		identity string
	}{
		{"wrong_identity", cipher, wrongIdentity},
		{"truncated_ciphertext", cipher[:len(cipher)-1], identity},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decryptRecovery(t.Context(), test.cipher, test.identity)
			defer clear(got)
			if err == nil || len(got) != 0 || strings.Contains(err.Error(), string(key)) || strings.Contains(err.Error(), string(rootToken)) {
				t.Fatal("invalid recovery input succeeded or disclosed plaintext")
			}
		})
	}
}

func realAgeIdentity(t *testing.T) (string, string) {
	t.Helper()
	for _, name := range []string{"age", "age-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip("age and age-keygen are integration-test prerequisites")
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("cannot restrict identity directory")
	}
	identity := filepath.Join(dir, "identity")
	cmd := exec.CommandContext(t.Context(), "age-keygen", "-o", identity)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		t.Fatal("cannot generate private age identity")
	}
	cmd = exec.CommandContext(t.Context(), "age-keygen", "-y", identity)
	cmd.Stderr = io.Discard
	recipient, err := cmd.Output()
	if err != nil {
		t.Fatal("cannot derive public age recipient")
	}
	return identity, strings.TrimSpace(string(recipient))
}

// A failure immediately after init must still leave both credentials encrypted
// on disk. No plaintext fixture is written, even before encryption.
func ageInstallRecoveryFixture(t *testing.T) (string, string, []byte, []byte) {
	t.Helper()
	identity, recipient := realAgeIdentity(t)
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		t.Fatal("cannot generate recovery fixture")
	}
	defer clear(random)
	rootToken := []byte(hex.EncodeToString(random[:32]))
	key := []byte(base64.StdEncoding.EncodeToString(random[32:]))
	path := filepath.Join(filepath.Dir(identity), "secrets.age")
	cfg := Config{InstallSecretStore: true, SecretsFile: path, SecretsAgeRecipient: recipient}
	if err := cfg.Validate(); err != nil {
		t.Fatal("invalid install fixture")
	}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err != nil {
		t.Fatal("real age recipient preflight failed")
	}
	secrets, err := createSecretsFile(path)
	if err != nil {
		t.Fatal("cannot create private install recovery file")
	}
	t.Cleanup(func() { _ = secrets.file.Close(); clear(secrets.plaintext) })
	if err := secrets.enableEncryption(t.Context(), recipient); err != nil {
		t.Fatal("cannot enable install encryption")
	}
	responses := freshStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{out: `{"initialized":false,"sealed":true,"storage_type":"file"}`}
	initBody, err := json.Marshal(map[string]any{"unseal_keys_b64": []string{string(key)}, "root_token": string(rootToken)})
	if err != nil {
		t.Fatal("cannot encode private init response")
	}
	defer clear(initBody)
	responses["operator init -key-shares=1 -key-threshold=1 -format=json"] = fakeBaoResponse{out: string(initBody)}
	responses["write -format=json sys/unseal -"] = fakeBaoResponse{err: errors.New("interrupted after init")}
	var output bytes.Buffer
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, &output, runningOpenBaoPod())
	creds := heldCredentials{atOnce: secrets.add}
	if _, err := setupProductionSecretStoreCollect(t.Context(), cfg, deps, deps, productionOpenBaoResult(), &creds); err == nil {
		t.Fatal("expected interruption after init")
	}
	cipher, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(cipher, []byte("age-encryption.org/")) || bytes.Contains(cipher, rootToken) || bytes.Contains(cipher, key) {
		t.Fatal("install did not persist encrypted recovery before interruption")
	}
	secrets.finish(&output)
	emitCredentials(&creds, deps, &output, false)
	if secrets.err != nil || bytes.Contains(output.Bytes(), rootToken) || bytes.Contains(output.Bytes(), key) {
		t.Fatal("install encryption failed or disclosed recovery material")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("encrypted recovery file is not owner-only")
	}
	return path, identity, rootToken, key
}
