package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
)

func TestFirstAdminCachedKeyWithoutCSRCanResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	configRoot := filepath.Join(root, "oberth")
	if err := os.Mkdir(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	enrollment, err := newInstallerEnrollment()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(enrollment.key)
	bootstrapCredentialInventory(t, enrollment.key)
	defer bootstrapPrivateFiles(t, root)
	defer bootstrapInstallerEnrollments(t, configRoot)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(files["ca.key"])
	bootstrapCredentialInventory(t, files["ca.key"])
	authority, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	certificate, _, err := authority.Issue("admin@host", enrollment.csr, 0)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{ContextName: "selected", RestConfig: &rest.Config{Host: "https://api.example.test"}, ClientConfigRoot: func() (string, error) { return configRoot, nil }, UserHomeDir: func() (string, error) { return root, nil }, Output: io.Discard, IsTerminal: func() bool { return false }, PollInterval: time.Millisecond}
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return json.Marshal(api.FirstAdminResult{InstallerCertificate: string(certificate), ServerCA: string(files["ca.crt"])})
	}
	if _, err := createFirstAdmin(t.Context(), Config{enrollment: enrollment}, deps, []byte("public"), "admin@host"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(configRoot, pendingInstallerName(Config{}, deps))); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset(readyOberthPod(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}})
	lists := 0
	kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		lists++
		if lists == 2 {
			return true, nil, errors.New("probe unavailable")
		}
		return false, nil, nil
	})
	deps.KubeClient = kube
	requests := 0
	deps.RunCommand = func(_ context.Context, body []byte, _ string, _ ...string) ([]byte, error) {
		requests++
		var request api.FirstAdminRequest
		if json.Unmarshal(body, &request) != nil || request.InstallerCSR == "" || request.InstallerCSR == string(enrollment.csr) {
			t.Fatal("cached key without CSR was reused", request.InstallerCSR)
		}
		return []byte(`{"token":"do-not-publish-stdout"}`), &exec.ExitError{Stderr: []byte("oberth: an active unscoped admin exists; retry with the original installer key or use authenticated administrative APIs")}
	}
	public, private := newEd25519HostKey(t)
	bootstrapCredentialInventory(t, private)
	defer clear(private)
	publicPath := filepath.Join(root, "operator.pub")
	if err := os.WriteFile(publicPath, ssh.MarshalAuthorizedKey(public), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Namespace: DefaultNamespace, existingRelease: true, SSHPublicKeyPath: publicPath, AdminSSHPublicKeyPath: publicPath, UplinkIdentity: "admin@host", Timeout: time.Second, ClientAccess: "none"}
	var output bytes.Buffer
	tw := newTableWriter(&output, false)
	tw.WriteHeader()
	tw.WriteFooter()
	err = FinishInstall(t.Context(), cfg, deps, tw, &heldCredentials{})
	if requests != 1 || !errors.Is(err, ErrOnboardingPartial) || !strings.Contains(err.Error(), "an active unscoped admin exists") || strings.Contains(err.Error(), "do-not-publish-stdout") {
		t.Fatal("cached enrollment recovery lost refusal/partial result", requests, err)
	}
}
