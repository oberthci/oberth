package installer_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd/api"
)

func TestReadinessWaitAnnouncedAndBounded(t *testing.T) {
	t.Parallel()
	for _, stop := range []bool{false, true} {
		t.Run(boolText(stop), func(t *testing.T) {
			readinessCase(t, stop)
		})
	}
}

func readinessCase(t *testing.T, stop bool) {
	t.Helper()
	f := setup(t)
	p := prepare(t, f)
	calls, pauses := 0, 0
	waiting := false
	f.engine.Timeout = time.Minute
	f.engine.CheckEndpoint = func(ctx context.Context, _ installer.Profile) error {
		if !waiting {
			t.Fatal("readiness was not announced")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded readiness")
		}
		calls++
		codes := []string{"starting", "starting", "connect"}
		if calls <= len(codes) {
			return installer.NewProblem(codes[calls-1], nil, "")
		}
		return nil
	}
	f.engine.Pause = func(context.Context) error {
		pauses++
		if stop {
			return context.DeadlineExceeded
		}
		return nil
	}
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if strings.HasPrefix(string(phase), "waiting for server") {
			waiting = true
		}
		return nil
	})
	if stop {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "timeout" || calls != 1 {
			t.Fatal(err, calls)
		}
	} else if err != nil || !reflect.DeepEqual([]int{calls, pauses}, []int{4, 3}) {
		t.Fatal(err, calls, pauses)
	}
}

func TestAdminKeyReuseAndCertificateRefresh(t *testing.T) {
	t.Parallel()
	f := setup(t)
	first := apply(t, f, prepare(t, f))
	raw, err := os.ReadFile(first.Profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	before := f.charts.Applies[0].Values["adminPublicKeySHA256"]
	f.engine.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }
	f.client.ClearActions()
	second := apply(t, f, prepare(t, f))
	again, err := os.ReadFile(second.Profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(again)
	if !bytes.Equal(raw, again) || before != f.charts.Applies[1].Values["adminPublicKeySHA256"] {
		t.Fatal("healthy rerun changed server key/checksum")
	}
	for _, a := range f.client.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "secrets" {
			t.Fatal("healthy key republished", a)
		}
	}
	if _, err = os.Stat(first.Profile.AdminCert); !os.IsNotExist(err) {
		t.Fatal("old certificate generation retained", err)
	}
}
func TestPluginCommandIsReviewedWithoutArguments(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.engine.Cluster.Config.ExecProvider = &api.ExecConfig{Command: "oberth-login", Args: []string{"private-argument"}, Env: []api.ExecEnvVar{{Name: "PRIVATE", Value: "private-value"}}}
	p := prepare(t, f)
	text := strings.Join(p.Review(), "\n") + strings.Join(p.Detail(), "\n")
	if !strings.Contains(text, "Login plugin: oberth-login") || strings.Contains(text, "private-argument") || strings.Contains(text, "private-value") {
		t.Fatal(text)
	}
}

func TestContextChangeRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Context = "another"
	if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		p.Close()
		t.Fatal("wrong context accepted")
	}
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal(a)
		}
	}
}

func TestHostAndForgeIdentityLossRefuses(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"oberth-ssh-host", "oberth-forge-key"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			apply(t, f, prepare(t, f))
			if err := f.client.CoreV1().Secrets("oberth").Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil {
				p.Close()
				t.Fatal("identity silently recreated")
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatal("missing secret not identified", err)
			}
		})
	}
}

func TestStoreLeafMustChainToInstallationCA(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-store-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(secret.Data["tls.crt"])
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(secret.Data["tls.key"])
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signer := key.(*ecdsa.PrivateKey)
	raw, err := x509.CreateCertificate(f.engine.Random, leaf, leaf, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["tls.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		p.Close()
		t.Fatal("independent leaf accepted despite valid key pair")
	}
}

func TestReleaseImageInventoryAndAppVersion(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"extra image", "chart version", "product digest"} {
		t.Run(kind, func(t *testing.T) {
			f := releaseFixture(t)
			var record map[string]any
			if err := json.Unmarshal(f.files["release.json"].Data, &record); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "extra image":
				record["images"].(map[string]any)["extra"] = "sha256:" + strings.Repeat("d", 64)
			case "chart version":
				record["version"] = "v0.2.0"
				f.version = "v0.2.0"
			case "product digest":
				record["images"].(map[string]any)["oberth"] = "sha256:" + strings.Repeat("d", 64)
			}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			f.files["release.json"].Data = body
			signRelease(t, f.files)
			if _, err = installer.LoadRelease(t.Context(), f.files, f.version); err == nil {
				t.Fatal("release metadata mismatch accepted")
			}
		})
	}
}

func TestReusedAdminKeyCannotOverwriteChangedSecret(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	p := prepare(t, f)
	secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["admin.pub"] = []byte("changed after review")
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	if _, err = f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("unreviewed key overwritten")
	}
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal("key refusal wrote cluster state", a)
		}
	}
}

func TestExternalStoreAddressIsReviewed(t *testing.T) {
	t.Parallel()
	f := setup(t)
	bundled := false
	f.choices.Store = &bundled
	f.choices.StoreAddress = "https://store.oberth.ci:8200"
	p := prepare(t, f)
	if !strings.Contains(strings.Join(p.Review(), "\n"), "external https://store.oberth.ci:8200 (--external-store URL)") {
		t.Fatal("external store target hidden", p.Review())
	}
}

func TestPushKeyMismatchRefusesReusedAdministrator(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["push.pub"] = []byte("another push key")
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		p.Close()
		t.Fatal("different push identity accepted")
	}
}

func TestMissingIdentityRemedyPreservesDatabase(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	if err := f.client.CoreV1().Secrets("oberth").Delete(t.Context(), "oberth-ca", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil {
		p.Close()
		t.Fatal("missing identity accepted")
	}
	if !strings.Contains(err.Error(), "oberth/oberth-ca") || !strings.Contains(err.Error(), "Apply a saved private copy of the named Secret") || !strings.Contains(err.Error(), "preserve the data volume and profile") || strings.Contains(err.Error(), "delete") || strings.Contains(err.Error(), "remove") {
		t.Fatal("missing identity has an unsafe or vague remedy", err)
	}
}
