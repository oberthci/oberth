package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type rejectedUnseal struct{ calls int }

func (s *rejectedUnseal) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	if r.URL.Path == "/v1/sys/seal-status" {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"initialized":true,"sealed":true}`))}, nil
	}
	if r.URL.Path != "/v1/sys/unseal" {
		return nil, errors.New("unexpected store request")
	}
	return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"errors"
 "encoding/json"
 "filippo.io/age":["private reply"]}`))}, nil
}
func TestUnsealRejectedKeyNamesInstallationRecovery(t *testing.T) {
	t.Parallel()
	f, _ := storeEngine(t)
	result := apply(t, f, prepare(t, f))
	transport := &rejectedUnseal{}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: transport})
		return c, func() {}, err
	}
	_, err := f.engine.Unseal(t.Context())
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Reason != "The unseal key does not match this store." || !strings.Contains(err.Error(), result.Profile.RecoveryKey) || strings.Contains(err.Error(), "private reply") || transport.calls != 2 {
		t.Fatal("unseal rejection omitted the recovery action", err)
	}
}
func TestUnsealChangedClusterRefusesBeforeOpeningStore(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	apply(t, f, prepare(t, f))
	namespace, err := f.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	namespace.UID = "cluster-two"
	if _, err = f.client.CoreV1().Namespaces().Update(t.Context(), namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	opened := false
	open := f.engine.OpenStore
	f.engine.OpenStore = func(ctx context.Context, cfg secretstore.Config) (*secretstore.Client, func(), error) {
		opened = true
		return open(ctx, cfg)
	}
	before := len(backend.Calls)
	_, err = f.engine.Unseal(t.Context())
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "profile-cluster" || opened || len(backend.Calls) != before {
		t.Fatal("another cluster received an unseal key", err)
	}
}
func TestLostStoreKeysNameDataVolume(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	backend.Initialized, backend.Sealed = true, true
	p := prepare(t, f)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "store-lost" || !strings.Contains(problem.Remedy, "data-oberth-store-0") || !strings.Contains(problem.Remedy, "install again") {
		t.Fatal("lost keys gave an unusable remedy", err)
	}
	for _, call := range backend.Calls {
		if call.Path == "/sys/unseal" || call.Path == "/sys/init" {
			t.Fatal("lost-key store was mutated")
		}
	}
}
func TestPendingStoreRootIsExplicitlyReviewed(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	first := prepare(t, f)
	backend.Fail = "/auth/token/revoke-self"
	if _, err := f.engine.Apply(t.Context(), first, first.Digest(), nil); err == nil {
		t.Fatal("fixture did not interrupt")
	}
	resumed := prepare(t, f)
	if !strings.Contains(strings.Join(resumed.Review(), "\n"), "Store setup resumes with the saved root token, which is then revoked.") || resumed.Digest() == first.Digest() {
		t.Fatal("root reuse lacks fresh review")
	}
	backend.Fail = ""
	if _, err := f.engine.Apply(t.Context(), resumed, first.Digest(), nil); err == nil {
		t.Fatal("old approval authorized root reuse")
	}
	apply(t, f, resumed)
}
func TestStoreCertificateLastsExactlyAsLongAsInstallationCA(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	ca, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh store leaf may be issued under an already established, older CA.
	later := setup(t)
	ca.ResourceVersion = ""
	if _, err = later.client.CoreV1().Secrets("oberth").Create(t.Context(), ca, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	later.engine.Now = func() time.Time { return f.engine.Now().AddDate(2, 0, 0) }
	apply(t, later, prepare(t, later))
	leaf, err := later.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-store-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := clientcert.ParseCertificate(ca.Data["tls.crt"])
	if err != nil {
		t.Fatal(err)
	}
	cert, err := clientcert.ParseCertificate(leaf.Data["tls.crt"])
	if err != nil || !cert.NotAfter.Equal(issuer.NotAfter) {
		t.Fatal("store leaf expires before the CA", err)
	}
}
func TestStoreRolesShareChartProjectedTokenAudience(t *testing.T) {
	t.Parallel()
	for _, audience := range []string{"oberth-store", "store-only"} {
		t.Run(audience, func(t *testing.T) {
			f, backend := storeEngine(t)
			open := f.engine.OpenStore
			var settings secretstore.Config
			f.engine.OpenStore = func(ctx context.Context, cfg secretstore.Config) (*secretstore.Client, func(), error) {
				settings = cfg
				return open(ctx, cfg)
			}
			f.choices.Values = map[string]any{"secretStore": map[string]any{"audience": audience}}
			apply(t, f, prepare(t, f))
			pod := serverDeployment(t, rendered(t, f.charts.Applies[0].Values)).Spec.Template.Spec
			projected := projectedStoreAudience(pod)
			cfg, err := decodeChartServer([]byte(jsonOf(t, serverJSON(t, rendered(t, f.charts.Applies[0].Values)))))
			if err != nil || cfg.SecretStore.Audience != projected || settings.Audience != projected {
				t.Fatal("server config and typed store constructor disagree with projection", err)
			}
			if projected != audience {
				t.Fatal("chart projects a different store audience")
			}
			for _, role := range []string{"oberth-writer", "oberth-step"} {
				if backend.Resources["/auth/kubernetes/role/"+role]["audience"] != projected {
					t.Fatal("store role differs from projected token audience", role)
				}
			}
		})
	}
}
func TestNativeStoreAndControllerSecurityContexts(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	var store appsv1.StatefulSet
	decodeObject(t, object(t, objects, "StatefulSet", "oberth-store"), &store)
	container := store.Spec.Template.Spec.Containers[0]
	security := container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		t.Fatal("store write or escalation policy differs")
	}
	assertDroppedCapabilities(t, security)
	assertRuntimeDefault(t, security.SeccompProfile)

	assertStoreTmp(t, store.Spec.Template.Spec)

	var controller appsv1.Deployment
	decodeObject(t, object(t, objects, "Deployment", "oberth-controller-workflow-controller"), &controller)
	assertRuntimeDefault(t, controller.Spec.Template.Spec.Containers[0].SecurityContext.SeccompProfile)
}
func decodeObject(t *testing.T, object map[string]any, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(jsonOf(t, object)), target); err != nil {
		t.Fatal(err)
	}
}

func projectedStoreAudience(pod corev1.PodSpec) string {
	for _, volume := range pod.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				return source.ServiceAccountToken.Audience
			}
		}
	}
	return ""
}

func assertDroppedCapabilities(t *testing.T, security *corev1.SecurityContext) {
	t.Helper()
	if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatal("store retains capabilities")
	}
}
func assertRuntimeDefault(t *testing.T, profile *corev1.SeccompProfile) {
	t.Helper()
	if profile == nil || profile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("native seccomp profile differs")
	}
}
func assertStoreTmp(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.MountPath != "/tmp" || mount.ReadOnly {
			continue
		}
		for _, volume := range pod.Volumes {
			if volume.Name == mount.Name && volume.EmptyDir != nil {
				return
			}
		}
	}
	t.Fatal("store cannot prepare its upstream listener configuration")
}

func TestPendingRootReviewRefusesUnsafeCustody(t *testing.T) {
	t.Parallel()
	f := setup(t)
	path := filepath.Join(f.dir, "store-init.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if p != nil {
		p.Close()
	}
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "custody" || !strings.Contains(err.Error(), path) {
		t.Fatal("unsafe saved root was accepted for review", err)
	}
}
func TestSealedStoreRefusesUnsafeRecoveryCustody(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	result := apply(t, f, prepare(t, f))
	backend.Sealed = true
	if err := os.Chmod(result.Profile.RecoveryKey, 0644); err != nil {
		t.Fatal(err)
	}
	p := prepare(t, f)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "custody" || !strings.Contains(err.Error(), result.Profile.RecoveryKey) {
		t.Fatal("unsafe recovery key gave a misleading remedy", err)
	}
}
