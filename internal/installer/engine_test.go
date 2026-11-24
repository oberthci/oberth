package installer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

type fixture struct {
	engine  *installer.Engine
	client  *kfake.Clientset
	charts  *fake.Chart
	release installer.Release
	choices installer.Choices
	dir     string
}

func setup(t *testing.T) fixture {
	t.Helper()
	client := kfake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-one"}})
	fake.Network(client)
	fake.DryRunServices(client)
	client.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.0"}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := credentials.Close(); err != nil {
			t.Error(err)
		}
	})
	release, err := installer.LocalRelease(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	charts := &fake.Chart{}
	e := &installer.Engine{Definitions: &fake.Definitions{}, Cluster: kubeconfig.Target{Context: "test", Config: &rest.Config{Host: "https://cluster.oberth.ci:6443"}, Client: client}, Charts: charts, Credentials: credentials, Random: rand.Reader, Now: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }, CheckEndpoint: func(context.Context, installer.Profile) error { return nil }}
	backend := &storefake.Store{}
	e.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() {}, err
	}
	return fixture{engine: e, client: client, charts: charts, release: release, choices: installer.Choices{Context: "test"}, dir: dir}
}
func prepare(t *testing.T, f fixture) *installer.Plan {
	t.Helper()
	p, err := f.engine.Prepare(context.Background(), f.release, f.choices)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
func apply(t *testing.T, f fixture, p *installer.Plan) installer.Result {
	t.Helper()
	r, err := f.engine.Apply(context.Background(), p, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Covers: REQ-C9-001
func TestResumablePrivateEnrollmentContract(t *testing.T) {
	t.Parallel()
	for _, phase := range []installer.Phase{installer.Checking, installer.ReleaseChecked, installer.Creating, installer.Saving, installer.Installing, installer.Waiting, installer.Endpoint, installer.Installed} {
		t.Run(string(phase), func(t *testing.T) {
			resumeAfterPhase(t, phase)
		})
	}
	t.Run("missing administrator key refuses", func(t *testing.T) {
		f := setup(t)
		r := apply(t, f, prepare(t, f))
		if err := os.Remove(r.Profile.AdminKey); err != nil {
			t.Fatal(err)
		}
		f.client.ClearActions()
		if _, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
			t.Fatal("missing owner key was replaced")
		}
		for _, action := range f.client.Actions() {
			if !readOnly(action) {
				t.Fatal("missing owner key caused writes")
			}
		}
	})
}

func TestPlanAndRevalidationAreReadOnly(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal(a)
		}
	}
	files, err := os.ReadDir(f.dir)
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	if _, err = f.engine.Apply(context.Background(), p, "", nil); err == nil {
		t.Fatal("unreviewed write")
	}
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = "other"
	if _, err = f.client.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(context.Background(), p, p.Digest(), nil); err == nil {
		t.Fatal("target change accepted")
	}
	files, err = os.ReadDir(f.dir)
	if err != nil || len(files) != 0 {
		t.Fatal("target refusal wrote credentials", files, err)
	}
}

func TestIdentitiesOutsideRelease(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := apply(t, f, prepare(t, f))
	first, err := f.client.CoreV1().Secrets("oberth").Get(context.Background(), "oberth-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range f.charts.Applies {
		raw := jsonOf(t, request.Values)
		if strings.Contains(raw, "PRIVATE KEY") || strings.Contains(raw, "AGE-SECRET-KEY") {
			t.Fatal("private bytes in values")
		}
	}
	apply(t, f, prepare(t, f))
	second, err := f.client.CoreV1().Secrets("oberth").Get(context.Background(), "oberth-ca", metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(first.Data, second.Data) {
		t.Fatal("identity changed", err)
	}
	if !strings.Contains(r.BackupReminder, "off this machine") {
		t.Fatal(r.BackupReminder)
	}
	if _, err := os.Stat(r.Profile.BackupKey); err != nil {
		t.Fatal(err)
	}
}

// Covers: REQ-C9-003
func TestReviewedUpgradeStateContract(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History.Deployed.Values["runs"] = map[string]any{"parallel": 0}
	f.charts.History.Deployed.Values["policies"] = map[string]any{"floor": false, "egress": false}
	f.charts.History.Latest = installer.Revision{Number: 2, Status: "failed", Kind: "local", Version: f.release.Version(), Values: map[string]any{"runs": map[string]any{"parallel": 99}}}
	p := prepare(t, f)
	review := strings.Join(p.Review(), "\n") + strings.Join(p.Detail(), "\n")
	for _, want := range []string{"failed", "failed_revision_values", "99", `"parallel": 0`, `"floor": false`, `"egress": false`, `"ttl_seconds": 1200`} {
		if !strings.Contains(review, want) {
			t.Fatal("review missing", want, review)
		}
	}
	if f.charts.Previews[len(f.charts.Previews)-1].Values["runs"].(map[string]any)["parallel"] != 0 {
		t.Fatal("failed revision values reused")
	}
}

// Covers: REQ-C9-038
func TestVerifiedReleaseInstallationContract(t *testing.T) {
	t.Parallel()

	f := releaseFixture(t)
	r := loadFixture(t, f)
	if !r.Verified() {
		t.Fatal("canonical release unverified")
	}
	for _, name := range []string{"SHA256SUMS.sigstore.json", "oberth.tgz", "release.json"} {
		t.Run(name, func(t *testing.T) {
			changed := releaseFixture(t)
			offset := 0
			if name == "oberth.tgz" {
				offset = 9
			}
			changed.files[name].Data[offset] ^= 1
			if _, err := installer.LoadRelease(t.Context(), changed.files, changed.version); err == nil {
				t.Fatal("tampered release accepted")
			}
		})
	}
	local, err := installer.LocalRelease(chartPath())
	if err != nil || local.Verified() {
		t.Fatal("local chart trust", err)
	}

}

func TestInstalledIdentityMissingRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	if err := f.client.CoreV1().Secrets("oberth").Delete(context.Background(), "oberth-ca", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Prepare(context.Background(), f.release, f.choices); err == nil {
		t.Fatal("missing established identity replaced")
	}
}

func TestAdminCertificateMatchesPrivateKey(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := apply(t, f, prepare(t, f))
	raw, err := os.ReadFile(r.Profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("private key missing")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(r.Profile.AdminCert)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := clientcert.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.(ed25519.PrivateKey).Public(), cert.PublicKey) {
		t.Fatal("certificate key mismatch")
	}
}

func TestRerunKeepsCustomEndpoints(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.URL = "https://localhost:34443"
	f.choices.SSHPort = 34022
	apply(t, f, prepare(t, f))
	f.choices.URL = ""
	f.choices.SSHPort = 0
	r := apply(t, f, prepare(t, f))
	if r.Profile.ServerURL != "https://localhost:34443" || r.Profile.SSHURL != "ssh://git@localhost:34022" {
		t.Fatal(r.Profile)
	}
}

func TestReleaseRevisionChangeRefusesBeforeCredentials(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	f.charts.History.Latest.Number = 2
	if _, err := f.engine.Apply(context.Background(), p, p.Digest(), nil); err == nil {
		t.Fatal("changed release applied")
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func resumeAfterPhase(t *testing.T, phase installer.Phase) {
	t.Helper()
	f := setup(t)
	p := prepare(t, f)
	cut := errors.New("interrupted")
	_, err := f.engine.Apply(context.Background(), p, p.Digest(), func(got installer.Phase) error {
		if got == phase || phase == installer.Waiting && strings.HasPrefix(string(got), string(phase)) {
			return cut
		}
		return nil
	})
	if !errors.Is(err, cut) {
		t.Fatal(err)
	}
	rerun := prepare(t, f)
	r := apply(t, f, rerun)
	assertAdminProfile(t, f, r)
	if len(f.charts.Applies) > 0 && !reflect.DeepEqual(f.charts.Previews[len(f.charts.Previews)-1].Values, f.charts.Applies[len(f.charts.Applies)-1].Values) {
		t.Fatal("preview/apply differ")
	}
}
func assertAdminProfile(t *testing.T, f fixture, r installer.Result) {
	t.Helper()
	raw, err := os.ReadFile(r.Profile.AdminCert)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := clientcert.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	caRaw, err := os.ReadFile(r.Profile.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.ParseCertificate(caRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err = cert.CheckSignatureFrom(ca); err != nil {
		t.Fatal(err)
	}
	secret, err := f.client.CoreV1().Secrets("oberth").Get(context.Background(), "oberth-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 2 {
		t.Fatal("bootstrap differs from saved key")
	}
	push, err := ssh.NewPublicKey(cert.PublicKey)
	if err != nil || !bytes.Equal(secret.Data["admin.pub"], ssh.MarshalAuthorizedKey(push)) || !bytes.Equal(secret.Data["push.pub"], ssh.MarshalAuthorizedKey(push)) {
		t.Fatal("public push key differs from saved administrator key", err)
	}
}

func TestClosedPlanCannotPublishErasedIdentities(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	approval := p.Digest()
	p.Close()
	if _, err := f.engine.Apply(t.Context(), p, approval, nil); err == nil {
		t.Fatal("closed plan applied")
	}
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal("closed plan wrote", a)
		}
	}
}

func readOnly(action ktesting.Action) bool {
	return action.GetVerb() == "get" || action.GetVerb() == "list" || fake.DryRunService(action)
}
