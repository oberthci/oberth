package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func storeEngine(t *testing.T) (fixture, *storefake.Store) {
	t.Helper()
	f := setup(t)
	backend := &storefake.Store{}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() {}, err
	}
	return f, backend
}
func TestStoreRootRevokedBeforeSuccessAndManualUnseal(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	r := apply(t, f, prepare(t, f))
	if !backend.Revoked {
		t.Fatal("root token not revoked")
	}
	if backend.Calls[len(backend.Calls)-1].Path != "/auth/token/revoke-self" {
		t.Fatal("success before revocation")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "store-init.json")); !os.IsNotExist(err) {
		t.Fatal("bootstrap root retained", err)
	}
	st, err := os.Stat(r.Profile.RecoveryKey)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("private recovery missing", err)
	}
	key, err := os.ReadFile(r.Profile.RecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	raw, _ := json.Marshal(r)
	if bytes.Contains(raw, key) || bytes.Contains(raw, []byte("temporary-bootstrap")) {
		t.Fatal("credential leaked in result")
	}
	testManualUnseal(t, f, backend, r)
}
func testManualUnseal(t *testing.T, f fixture, backend *storefake.Store, r installer.Result) {
	t.Helper()
	var err error
	backend.Sealed = true
	p := prepare(t, f)
	if _, err = f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("sealed rerun automatically opened store")
	}
	if _, err = f.engine.Unseal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if backend.Sealed {
		t.Fatal("manual unseal did not open store")
	}
	if err = os.Remove(r.Profile.RecoveryKey); err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Unseal(t.Context()); err == nil || !strings.Contains(err.Error(), r.Profile.RecoveryKey) {
		t.Fatal("missing key refusal omitted path", err)
	}
}

func TestInterruptedStoreSetupKeepsRecoveryAndRevokesOnRerun(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	backend.Fail = "/auth/token/revoke-self"
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("revocation failure reported success")
	}
	first, err := os.ReadFile(filepath.Join(f.dir, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first)
	if _, err = os.Stat(filepath.Join(f.dir, "store-init.json")); err != nil {
		t.Fatal("resumable initialization lost", err)
	}
	backend.Fail = ""
	apply(t, f, prepare(t, f))
	second, err := os.ReadFile(filepath.Join(f.dir, "recovery"))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("unseal key replaced", err)
	}
	clear(second)
	if !backend.Revoked {
		t.Fatal("root not revoked on resume")
	}
}
func TestExternalStoreNeverInitializedOrUnsealed(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	no := false
	f.choices.Store = &no
	f.choices.StoreAddress = "https://external.oberth.svc"
	apply(t, f, prepare(t, f))
	if len(backend.Calls) != 0 {
		t.Fatal("external store mutated")
	}
	if _, err := f.engine.Unseal(t.Context()); err == nil {
		t.Fatal("external unseal accepted")
	}
}
func TestStorePodMustMatchStatefulSetUID(t *testing.T) {
	t.Parallel()
	f := setup(t)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "oberth-store", Namespace: "oberth", UID: "store-owner", Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "store"}}}}
	if _, err := f.client.AppsV1().StatefulSets("oberth").Create(t.Context(), sts, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	yes := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "store-spoof", Namespace: "oberth", Labels: map[string]string{"app": "store"}, OwnerReferences: []metav1.OwnerReference{{UID: "other-owner", Controller: &yes}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if _, err := f.client.CoreV1().Pods("oberth").Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.StorePodForTest(t.Context(), "oberth"); err == nil {
		t.Fatal("label spoof accepted")
	}
	pod.Name = "store-owned"
	pod.OwnerReferences[0].UID = sts.UID
	if _, err := f.client.CoreV1().Pods("oberth").Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if name, err := f.engine.StorePodForTest(t.Context(), "oberth"); err != nil || name != pod.Name {
		t.Fatal(name, err)
	}
}

type secretPutFunc func(context.Context, installer.SecretPutRequest, []byte) error

func (f secretPutFunc) PutSecret(ctx context.Context, r installer.SecretPutRequest, v []byte) error {
	return f(ctx, r, v)
}
func TestSecretPutUsesOnlyBoundedServiceInput(t *testing.T) {
	t.Parallel()
	called := false
	port := secretPutFunc(func(_ context.Context, _ installer.SecretPutRequest, value []byte) error {
		called = true
		if string(value) != "value" {
			t.Fatal("stream changed")
		}
		return nil
	})
	if err := installer.SecretPut(t.Context(), port, installer.SecretPutRequest{}, strings.NewReader("value")); err != nil || !called {
		t.Fatal(err)
	}
	called = false
	if err := installer.SecretPut(t.Context(), port, installer.SecretPutRequest{}, strings.NewReader(strings.Repeat("x", secretstore.MaxValueBytes+1))); err == nil || called {
		t.Fatal("oversized input reached service")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := installer.SecretPut(ctx, port, installer.SecretPutRequest{}, strings.NewReader("value")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSecretPutAcceptsMaximumValue(t *testing.T) {
	t.Parallel()
	called := false
	port := secretPutFunc(func(_ context.Context, _ installer.SecretPutRequest, value []byte) error {
		called = true
		if len(value) != secretstore.MaxValueBytes {
			t.Fatal("maximum value changed")
		}
		return nil
	})
	if err := installer.SecretPut(t.Context(), port, installer.SecretPutRequest{}, strings.NewReader(strings.Repeat("x", secretstore.MaxValueBytes))); err != nil || !called {
		t.Fatal("maximum value refused", err)
	}
}
