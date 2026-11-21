package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
	installfake "github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kversion "k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/rest"
)

type repoAddCommand struct {
	app             application
	out, diagnostic *bytes.Buffer
	address, known  string
}

func (f *composedFixture) backupApplication(t *testing.T) (application, *age.X25519Identity) {
	t.Helper()
	base := t.TempDir()
	profileDir := filepath.Join(base, "installed")
	if err := os.Mkdir(profileDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(f.adminCertificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.ParseCertificate(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	profile := installer.Profile{ServerURL: f.config.Public.URL, Context: "fresh-install", Namespace: f.config.Namespace, CAPin: clientcert.Pin(ca), ServerVersion: binaryVersion(),
		AdminCert: filepath.Join(profileDir, "admin.crt"), AdminKey: filepath.Join(profileDir, "admin.key"), CAFile: filepath.Join(profileDir, "ca.crt"), BackupRecipient: key.Recipient().String()}
	metadata, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"installation.json": metadata, "admin.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.adminCertificate.Certificate[0]}), "admin.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), "ca.crt": f.ca} {
		if err = client.WritePrivate(filepath.Join(profileDir, name), value); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := client.PrepareProfileSelection(base, "installed")
	if err != nil {
		t.Fatal(err)
	}
	defer selection.Close()
	if err = selection.Commit(); err != nil {
		t.Fatal(err)
	}
	app := application{base: base, in: &bytes.Buffer{}, out: &bytes.Buffer{}, diagnostic: &bytes.Buffer{}, env: func(string) string { t.Error("backup read cluster environment"); return "" }, config: client.Config{Now: f.deps.clock.Now}}
	return app, key
}

func installComposedProfile(ctx context.Context, t *testing.T, f *composedFixture, port int) installer.Profile {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	_, err = f.kube.CoreV1().Namespaces().Create(ctx, &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: "installation-cluster"}}, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.kube.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &kversion.Info{GitVersion: "v1.30.0"}
	backend := &storefake.Store{}
	installfake.DryRunServices(f.kube)
	e := &installer.Engine{Definitions: &installfake.Definitions{}, Cluster: kubeconfig.Target{Context: "installed", Config: &rest.Config{Host: "https://127.0.0.1:6443"}, Client: f.kube}, Charts: &installfake.Chart{}, Credentials: credentials, Random: rand.Reader, Now: f.deps.clock.Now, CheckEndpoint: func(context.Context, installer.Profile) error { return nil }}
	e.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() {}, err
	}
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Prepare(ctx, release, installer.Choices{Context: "installed", URL: f.config.Public.URL, SSHPort: port})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, err = e.Apply(ctx, p, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mountInstalledIdentities(ctx, t, f)
	raw, err := os.ReadFile(filepath.Join(dir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile installer.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func mountInstalledIdentities(ctx context.Context, t *testing.T, f *composedFixture) {
	t.Helper()
	for name, paths := range map[string]map[string]string{
		"oberth-ca":       {"tls.crt": f.config.Files.CACertificate, "tls.key": f.config.Files.CAPrivateKey},
		"oberth-admin":    {"admin.pub": f.config.Files.AdminPublicKey, "push.pub": f.config.Files.AdminPushKey},
		"oberth-ssh-host": {"key": f.config.Files.SSHHostKey},
	} {
		s, err := f.kube.CoreV1().Secrets(f.config.Namespace).Get(ctx, name, meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for key, path := range paths {
			if err = os.WriteFile(path, s.Data[key], 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "oberth-ca" {
			f.ca, f.key = s.Data["tls.crt"], s.Data["tls.key"]
		}
	}
}

func (c *repoAddCommand) run(t *testing.T, flags ...string) int {
	t.Helper()
	c.out.Reset()
	c.diagnostic.Reset()
	return c.app.run(t.Context(), append([]string{"repo", "add", c.address, "--known-hosts", c.known}, flags...))
}
