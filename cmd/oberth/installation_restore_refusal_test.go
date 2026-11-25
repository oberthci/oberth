package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRestoreRefusalsPrecedeClusterWrites(t *testing.T) {
	for _, scenario := range []string{"approval", "source-cluster", "occupied-namespace", "administrator", "valid-different-administrator", "unseal", "corrupt", "release"} {
		t.Run(scenario, func(t *testing.T) { restoreRefusalCase(t, scenario) })
	}
}

func restoreRefusalCase(t *testing.T, scenario string) {
	t.Helper()
	f := newRestoreFixture(t)
	configureRestoreRefusal(t, f, scenario)
	f.target.client.ClearActions()
	if _, err := f.target.engine.Restore(t.Context(), f.config); err == nil || strings.Contains(err.Error(), "private-wrong") {
		t.Fatal("unsafe restore succeeded or exposed private input", scenario, err)
	}
	assertNoRestoreWrites(t, f.target)
	if len(f.target.charts.Applies) != 0 || f.storeApplied {
		t.Fatal("restore refusal changed the target installation")
	}
	if _, err := os.Stat(f.destination); !os.IsNotExist(err) {
		t.Fatal("restore refusal published a database", err)
	}
}

func configureRestoreRefusal(t *testing.T, f *restoreFixture, scenario string) {
	t.Helper()
	switch scenario {
	case "approval":
		f.config.Yes = false
	case "source-cluster":
		useOriginalCluster(t, f)
	case "occupied-namespace":
		if _, err := f.target.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	case "administrator":
		f.config.Administrator = []byte("private-wrong-identity")
	case "valid-different-administrator":
		f.config.Administrator = differentRestoreAdministrator(t)
	case "unseal":
		f.config.Unseal = nil
	case "corrupt":
		corruptRestoreArchive(t, f.config.Archive.Path)
	case "release":
		f.config.Release = installer.Release{}
	}
}

func differentRestoreAdministrator(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(der)
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	t.Cleanup(func() { clear(raw) })
	return raw
}

func useOriginalCluster(t *testing.T, f *restoreFixture) {
	t.Helper()
	ns, err := f.target.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.source.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = original.UID
	if _, err = f.target.client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func corruptRestoreArchive(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreTransferFailureKeepsServerStopped(t *testing.T) {
	f := newRestoreFixture(t)
	f.target.engine.RestoreDatabase = func(context.Context, string, stateport.Options, io.Reader) (stateport.BackupReport, error) {
		return stateport.BackupReport{}, errors.New("private-stream-failure")
	}
	if _, err := f.target.engine.Restore(t.Context(), f.config); err == nil || strings.Contains(err.Error(), "private-stream-failure") {
		t.Fatal("failed database transfer was exposed or accepted", err)
	}
	if len(f.target.charts.Applies) != 1 || f.target.charts.Applies[0].Values["restorePending"] != true {
		t.Fatal("database transfer failure started the server")
	}
}
