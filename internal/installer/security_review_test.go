package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

func TestFailedOpenBaoInitKeepsCredentialsOutOfErrorsAndDeliversSink(t *testing.T) {
	responses := freshStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{out: `{"initialized":false,"sealed":true,"storage_type":"file"}`}
	responses["operator init -key-shares=1 -key-threshold=1 -format=json"] = fakeBaoResponse{out: `{"root_token":"fixture-root-secret","unseal_keys_b64":["fixture-unseal-secret"]}`, err: errors.New("exit status 1")}
	runner := &fakeBaoRunner{t: t, responses: responses}
	var output bytes.Buffer
	deps := secretStoreTestDeps(runner, &output, runningOpenBaoPod())
	received := map[string]string{}
	deps.CredentialSink = func(label, value string) { received[label] = value }
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var held heldCredentials
	_, err := setupProductionSecretStoreCollect(t.Context(), cfg, deps, deps, productionOpenBaoResult(), &held)
	emitCredentials(&held, deps, &output, false)
	if err == nil {
		t.Fatal("failed init succeeded")
	}
	for _, secret := range []string{"fixture-root-secret", "fixture-unseal-secret"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(output.String(), secret) {
			t.Fatalf("init secret escaped: %v output=%s", err, &output)
		}
	}
	if received["Root token"] != "fixture-root-secret" || received["Unseal key"] != "fixture-unseal-secret" {
		t.Fatalf("credentials lost: labels=%d", len(received))
	}
}
func TestOpenBaoPodRequiresLiveControllerUID(t *testing.T) {
	for _, which := range []string{"absent", "wrong-uid", "not-controller", "valid"} {
		t.Run(which, func(t *testing.T) {
			pod := runningOpenBaoPod()
			pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "openbao", UID: types.UID("live-controller"), Controller: ptr.To(true)}}
			switch which {
			case "absent":
				pod.OwnerReferences = nil
			case "wrong-uid":
				pod.OwnerReferences[0].UID = "other"
			case "not-controller":
				pod.OwnerReferences[0].Controller = ptr.To(false)
			}
			set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "openbao", Namespace: DefaultOpenBaoNamespace, UID: "live-controller"}}
			_, err := findOpenBaoPod(t.Context(), Deps{KubeClient: fake.NewClientset(pod, set)}, DefaultOpenBaoNamespace)
			if (err == nil) != (which == "valid") {
				t.Fatalf("%s: %v", which, err)
			}
		})
	}
}
func TestServerPoliciesAndPathsKeepInstallIdentitiesIsolated(t *testing.T) {
	for _, policy := range []string{OberthPolicy("oberth", "install-a"), OberthProductionPolicy("oberth", "transit", "key", "install-a")} {
		if strings.Contains(policy, `path "oberth/data/*"`) {
			t.Fatal("server can read another install's identities")
		}
		if !strings.Contains(policy, `path "oberth/data/identities/install-a/*"`) {
			t.Fatal("own identity path missing")
		}
	}
	for _, path := range []string{".", "..", "release/../identities/other/server", "release/./key"} {
		if validateSecretPath(path) == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}
func TestRunRemovesBaoTokenBeforeChildCommands(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "fixture-admin-token")
	deps := Deps{Output: io.Discard, RestConfig: &rest.Config{Host: "https://remote.example"}, KubeClient: fake.NewClientset(), RunHelm: func(context.Context, []string) ([]byte, error) { return nil, errors.New("stop") }}
	_ = Run(t.Context(), Config{}, deps)
	if value := os.Getenv(baoTokenEnvVar); value != "" {
		t.Fatal("BAO_TOKEN left in subprocess environment")
	}
}

func TestDeployKeyInputIsBounded(t *testing.T) {
	name := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(name, bytes.Repeat([]byte("x"), 1<<20|1), 0600); err != nil {
		t.Fatal(err)
	}
	if body, err := readDeployKey(name); err == nil {
		clear(body)
		t.Fatal("oversized private key accepted")
	}
}

func TestDefaultChildCommandsDoNotInheritAdminTokens(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "fixture-bao-secret")
	t.Setenv("VAULT_TOKEN", "fixture-vault-secret")
	out, err := DefaultRunCommand(t.Context(), nil, "sh", "-c", `test -z "$BAO_TOKEN" && test -z "$VAULT_TOKEN"`)
	if err != nil {
		t.Fatalf("child inherited token: %v (%d output bytes)", err, len(out))
	}
}

func TestFailedOpenBaoInitClearsBothCapturedStreams(t *testing.T) {
	combined := []byte(`{"root_token":"fixture-root","unseal_keys_b64":["fixture-key"]}`)
	stderr := []byte("fixture-sensitive-stderr")
	childErr := &exec.ExitError{Stderr: stderr}
	client := openBaoExec{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return combined, errors.Join(errors.New("command failed"), childErr)
	}}
	result, err := client.operatorInit(t.Context())
	if err == nil || result.RootToken != "fixture-root" || len(result.UnsealKeysB64) != 1 {
		t.Fatalf("failed init lost parsed credentials: err=%v", err)
	}
	if !bytes.Equal(combined, make([]byte, len(combined))) || !bytes.Equal(stderr, make([]byte, len(stderr))) {
		t.Fatal("failed init retained captured credential bytes")
	}
}
