package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func recoveryFixture(t *testing.T) (string, []byte) {
	t.Helper()
	key := []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 32)))
	path := filepath.Join(t.TempDir(), "recovery")
	if err := os.WriteFile(path, append([]byte("OPENBAO_UNSEAL_KEY="), key...), 0600); err != nil {
		t.Fatal(err)
	}
	return path, key
}

func TestUnsealKeepsRecoveryOutOfOutputAndArgv(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			path, key := recoveryFixture(t)
			sealed := true
			writes := 0
			var inputCopy []byte
			deps := Deps{KubeClient: fake.NewClientset(runningOpenBaoPod(), runningOpenBaoStatefulSet()), ContextName: "chosen"}
			deps.RunCommand = func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
				if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--context chosen") {
					t.Fatal("lost selected context")
				}
				if bytes.Contains([]byte(strings.Join(args, " ")), key) {
					t.Fatal("recovery key entered argv")
				}
				if strings.Contains(strings.Join(args, " "), "sys/unseal") {
					writes++
					inputCopy = input
					var payload map[string]string
					if json.Unmarshal(input, &payload) != nil || payload["key"] != string(key) {
						t.Fatal("invalid private request body")
					}
					if fail {
						return bytes.Clone(key), errors.New(string(key))
					}
					sealed = false
					return bytes.Clone(key), nil
				}
				body, _ := json.Marshal(SealStatus{Initialized: true, Sealed: sealed})
				return body, nil
			}
			status, err := unsealWithDeps(t.Context(), UnsealConfig{OpenBaoNamespace: "openbao", SecretsFile: path}, deps)
			if (err != nil) != fail || writes != 1 {
				t.Fatal("unexpected unseal result")
			}
			var output bytes.Buffer
			_ = PrintSealStatus(&output, status, true)
			if bytes.Contains(output.Bytes(), key) || (err != nil && strings.Contains(err.Error(), string(key))) {
				t.Fatal("recovery material escaped")
			}
			if len(bytes.Trim(inputCopy, "\x00")) != 0 {
				t.Fatal("request body retained after unseal")
			}
		})
	}
}

func TestRecoveryInputRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "hardlink", "duplicate", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			path, key := recoveryFixture(t)
			switch kind {
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := path + "-link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "hardlink":
				if err := os.Link(path, path+"-link"); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				body := append([]byte("OPENBAO_UNSEAL_KEY="), key...)
				body = append(body, '\n')
				body = append(body, body...)
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte("OPENBAO_UNSEAL_KEY=invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readRecoveryUnsealKey(t.Context(), path, ""); err == nil {
				t.Fatal("unsafe recovery input accepted")
			}
		})
	}
}

func TestUnsealReadsAgeRecoveryInMemory(t *testing.T) {
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen is a test prerequisite")
	}
	path, key := recoveryFixture(t)
	identity := filepath.Join(filepath.Dir(path), "identity")
	cmd := exec.Command("age-keygen", "-o", identity)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		t.Fatal("identity generation failed")
	}
	recipient, err := exec.Command("age-keygen", "-y", identity).Output()
	if err != nil {
		t.Fatal("cannot read public recipient")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(body)
	cmd = exec.Command("age", "-r", strings.TrimSpace(string(recipient)))
	cmd.Stdin = bytes.NewReader(body)
	var cipher bytes.Buffer
	cmd.Stdout = &cipher
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		t.Fatal("encryption failed")
	}
	if err := os.WriteFile(path, cipher.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := readRecoveryUnsealKey(t.Context(), path, identity)
	defer clear(actual)
	if err != nil || !bytes.Equal(actual, key) {
		t.Fatal("private age roundtrip failed")
	}
	if _, err := readRecoveryUnsealKey(t.Context(), path, path+"-missing"); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestTPMUnsealRefusesUnsupportedHostsAndHasNoFallback(t *testing.T) {
	one := corev1.Node{}
	one.Status.NodeInfo.MachineID = "fixture-machine"
	if validateAutoUnsealNode([]corev1.Node{one}, "fixture-machine") != nil {
		t.Fatal("same host refused")
	}
	for _, nodes := range [][]corev1.Node{nil, {one, one}} {
		if validateAutoUnsealNode(nodes, "fixture-machine") == nil {
			t.Fatal("unsupported topology accepted")
		}
	}
	if validateAutoUnsealNode([]corev1.Node{one}, "other-machine") == nil {
		t.Fatal("remote node accepted")
	}
	one.Spec.ProviderID = "cloud://managed"
	if validateAutoUnsealNode([]corev1.Node{one}, "fixture-machine") == nil {
		t.Fatal("managed node accepted")
	}
	_, key := recoveryFixture(t)
	path := filepath.Join(t.TempDir(), "bound.cred")
	err := writeTPMCredential(t.Context(), path, key, func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name != "systemd-creds" || !strings.Contains(strings.Join(args, " "), "--with-key=tpm2") || bytes.Contains([]byte(strings.Join(args, " ")), key) {
			t.Fatal("TPM2/key transport contract violated")
		}
		if !bytes.Contains(input, key) {
			t.Fatal("missing stdin payload")
		}
		return bytes.Clone(key), errors.New("no TPM2")
	})
	if err == nil || strings.Contains(err.Error(), string(key)) {
		t.Fatal("missing TPM2 was not safely refused")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed sealing left a credential file")
	}
}

func TestUnsealStatusDoesNotReadRecovery(t *testing.T) {
	deps := Deps{KubeClient: fake.NewClientset(runningOpenBaoPod(), runningOpenBaoStatefulSet()), RunCommand: func(_ context.Context, input []byte, _ string, args ...string) ([]byte, error) {
		if len(input) > 0 || strings.Contains(strings.Join(args, " "), "sys/unseal") {
			t.Fatal("status tried to unseal")
		}
		return []byte(`{"initialized":true,"sealed":true}`), errors.New("sealed status exit")
	}}
	status, err := unsealWithDeps(t.Context(), UnsealConfig{OpenBaoNamespace: "openbao", StatusOnly: true, SecretsFile: "/missing/not-read"}, deps)
	if err != nil || !status.Sealed {
		t.Fatal("read-only status failed")
	}
}

func TestTPMCredentialUnitUsesPrivateCLI(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/systemd/oberth-openbao-unseal.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("LoadCredentialEncrypted=")) || bytes.Contains(unit, []byte("LoadCredential=")) {
		t.Fatal("boot unit lacks encrypted credentials")
	}
	script, err := os.ReadFile("../../deploy/systemd/oberth-openbao-unseal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(script, []byte("oberth unseal --secrets-file")) || bytes.Contains(script, []byte("cat ")) {
		t.Fatal("boot unit bypasses private unseal")
	}
}

func TestUnsealRefusesInsecureKubeconfigBeforeSecretRead(t *testing.T) {
	_, err := ExecuteUnseal(t.Context(), UnsealConfig{SecretsFile: "/missing"}, InstallDeps{
		LookPath: func(string) (string, error) { return "/tool", nil },
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			return fake.NewClientset(), &rest.Config{TLSClientConfig: rest.TLSClientConfig{Insecure: true}}, "chosen", nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("insecure cluster reached command execution")
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "TLS verification") {
		t.Fatal("insecure context accepted")
	}
}
