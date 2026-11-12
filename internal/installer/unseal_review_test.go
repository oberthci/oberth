package installer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestUnsealTrustGatePrecedesClusterAndRecoveryAccess(t *testing.T) {
	ca := newTestServerCA(t, []string{"api.example.test"})
	for _, config := range []*rest.Config{nil, {Host: "http://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, {Host: "https://api.example.test"}, {Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca, Insecure: true}}} {
		kube := fake.NewClientset()
		_, err := ExecuteUnseal(t.Context(), UnsealConfig{SecretsFile: "/missing/not-read"}, InstallDeps{
			LookPath:       func(name string) (string, error) { return name, nil },
			LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) { return kube, config, "chosen", nil },
			RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("untrusted cluster reached a subprocess")
				return nil, nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "explicit trusted cluster CA") || len(kube.Actions()) != 0 {
			t.Fatal("unseal bypassed cluster trust gate")
		}
	}
}

func TestUnsealRejectsFailedUnsealedStatus(t *testing.T) {
	deps := Deps{KubeClient: fake.NewClientset(runningOpenBaoPod(), runningOpenBaoStatefulSet()), RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte(`{"initialized":true,"sealed":false}`), errors.New("transport failed")
	}}
	if _, err := unsealWithDeps(t.Context(), UnsealConfig{OpenBaoNamespace: "openbao", StatusOnly: true}, deps); err == nil {
		t.Fatal("failed status claimed unsealed")
	}
}

// Port pane 4's real subprocess disclosure check onto the verified-pod path.
func TestUnsealSubprocessDoesNotExposeCredentials(t *testing.T) {
	path, key := recoveryFixture(t)
	defer clear(key)
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "public-process-state")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$UNSEAL_TEST_TRACE"
env >> "$UNSEAL_TEST_TRACE"
case " $* " in
 *" status "*) printf '%s\n' '{"initialized":true,"sealed":true}'; exit 2 ;;
 *) cat >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("UNSEAL_TEST_TRACE", trace)
	deps := Deps{KubeClient: fake.NewClientset(runningOpenBaoPod(), runningOpenBaoStatefulSet()), ContextName: "chosen", RunCommand: privateUnsealCommand}
	_, err := unsealWithDeps(t.Context(), UnsealConfig{SecretsFile: path, OpenBaoNamespace: "openbao"}, deps)
	if err == nil || strings.Contains(err.Error(), string(key)) {
		t.Fatal("unsafe subprocess failure")
	}
	state, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(state, key) {
		t.Fatal("secret appeared in argv or process environment")
	}
}

func TestTPMCredentialSuccessAndOverwriteRefusal(t *testing.T) {
	_, key := recoveryFixture(t)
	defer clear(key)
	path := filepath.Join(t.TempDir(), "bound.cred")
	calls := 0
	run := func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
		calls++
		if name != "systemd-creds" || !strings.Contains(strings.Join(args, " "), "--with-key=tpm2") || !bytes.Contains(input, key) || bytes.Contains([]byte(strings.Join(args, " ")), key) {
			t.Fatal("invalid credential transport")
		}
		return []byte("opaque encrypted credential"), nil
	}
	if err := writeTPMCredential(t.Context(), path, key, run); err != nil {
		t.Fatal("credential write failed")
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, key) {
		t.Fatal("plaintext credential saved")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential is not private")
	}
	if err := writeTPMCredential(t.Context(), path, key, run); err == nil || calls != 1 {
		t.Fatal("existing credential was not refused before encryption")
	}
}

func TestBootUnsealRequiresAndPreservesSelectedContext(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "oberth")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("../../deploy/systemd/oberth-openbao-unseal")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "boot")
	if err := os.WriteFile(script, bytes.ReplaceAll(body, []byte("/usr/local/bin/oberth"), []byte(cli)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", script)
	cmd.Env = []string{"CREDENTIALS_DIRECTORY=" + root}
	if out, err := cmd.CombinedOutput(); err == nil || bytes.Contains(out, []byte("--secrets-file")) {
		t.Fatal("boot selected an implicit context")
	}
	cmd = exec.CommandContext(t.Context(), "/bin/sh", script)
	cmd.Env = []string{"CREDENTIALS_DIRECTORY=" + root, "OPENBAO_CONTEXT=chosen cluster", "OPENBAO_NAMESPACE=chosen-ns"}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--context\nchosen cluster\n--openbao-namespace\nchosen-ns\n") {
		t.Fatal("boot lost selected coordinates")
	}
}
