package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func controllerUpgradeFixture(t *testing.T) (UpgradeConfig, Deps, *[][]string) {
	t.Helper()
	calls := [][]string{}
	cfg := UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.23", ArgoControllerProfile: argoworkflow.NonrootStaticProfile, Timeout: 10 * time.Millisecond, Yes: true}
	deps := Deps{Output: &bytes.Buffer{}, KubeClient: fake.NewClientset()}
	deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		switch args[0] {
		case "version":
			return []byte("v4.2.3"), nil
		case "template":
			return nil, nil
		case "list":
			if slices.Contains(args, "oberth") {
				return []byte(`[{"name":"oberth","chart":"oberth-0.17.23","status":"deployed","revision":82}]`), nil
			}
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed","revision":18}]`), nil
		case "get":
			if args[2] == "oberth" {
				return []byte(`{"compatibility":{"schemaIdentity":"legacy-schema-v1","auditDomain":"legacy-audit-v1","witnessKeyInfo":"legacy-witness-v1"},"argo":{"namespace":"oberth-argo","controllerProfile":"nonroot-static-v1"}}`), nil
			}
			return []byte(`{"singleNamespace":true,"server":{"enabled":false}}`), nil
		case "pull":
			dir := args[slices.Index(args, "--destination")+1]
			return nil, os.WriteFile(filepath.Join(dir, "argo-workflows-1.0.24.tgz"), []byte("public chart fixture"), 0o600)
		}
		return nil, nil
	}
	return cfg, deps, &calls
}

func TestUpgradeControllerProfileDryRunAndOverrides(t *testing.T) {
	cfg, deps, calls := controllerUpgradeFixture(t)
	cfg.DryRun = true
	result, err := RunUpgrade(t.Context(), cfg, deps)
	if err != nil || result.AlreadyUpToDate || result.Upgraded {
		t.Fatalf("explicit same-version preview refused: %+v %v", result, err)
	}
	output := deps.Output.(*bytes.Buffer).String()
	for _, wanted := range []string{"Controller transition:", argoworkflow.NonrootExecutorImage, "argo.controllerProfile=nonroot-static-v1", "No automatic rollback", "Reconcile the Argo controller profile at"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("preview omitted %s", wanted)
		}
	}
	for _, call := range *calls {
		if call[0] == "upgrade" || call[0] == "pull" {
			t.Fatal("dry-run mutated cluster or fetched chart")
		}
	}
	for _, key := range []string{"argo", "argo.namespace", "argo.controllerProfile"} {
		changed := cfg
		changed.UserSet = []string{key + "=other"}
		if _, err := RunUpgrade(t.Context(), changed, deps); err == nil {
			t.Fatal("conflicting profile scope accepted")
		}
	}
	cfg.ArgoControllerProfile = "unknown"
	if err := cfg.ValidateUpgrade(); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestUpgradeControllerProfilePreparationRefusesForeignState(t *testing.T) {
	for _, mode := range []string{"absent", "pending", "foreign-chart", "foreign-values", "namespace", "values-changed-between-reads"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, calls := controllerUpgradeFixture(t)
			base := deps.RunHelm
			gets := 0
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "list" {
					switch mode {
					case "absent":
						return []byte(`[]`), nil
					case "pending":
						return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"pending-upgrade","revision":18}]`), nil
					case "foreign-chart":
						return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.23","status":"deployed","revision":18}]`), nil
					}
				}
				if args[0] == "get" && args[2] == "oberth" && mode == "namespace" {
					return []byte(`{"argo":{"namespace":"oberth"}}`), nil
				}
				if args[0] == "get" && args[2] == "argo-workflows" {
					gets++
					if mode == "foreign-values" || (mode == "values-changed-between-reads" && gets == 2) {
						return []byte(`{"controller":{"extraEnv":[{"name":"INJECT","value":"canary"}]}}`), nil
					}
				}
				return base(ctx, args)
			}
			_, cleanup, err := prepareUpgradeControllerProfile(t.Context(), cfg, deps)
			cleanup()
			if err == nil {
				t.Fatal("unsupported profile transition accepted")
			}
			for _, call := range *calls {
				if call[0] == "upgrade" {
					t.Fatal("preparation mutated cluster")
				}
			}
		})
	}
}

func TestUpgradeControllerProfileMutationAndReadbackFailures(t *testing.T) {
	for _, mode := range []string{"revision-changed", "values-changed", "revision-during-pull", "values-during-pull", "bad-chart", "upgrade-failed", "readback-failed"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, calls := controllerUpgradeFixture(t)
			profile, cleanup, err := prepareUpgradeControllerProfile(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			sum := sha256.Sum256([]byte("public chart fixture"))
			profile.chartDigest = hex.EncodeToString(sum[:]) // test fixture only; production preparation fixes official digest.
			base := deps.RunHelm
			pulled := false
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "pull" {
					pulled = true
				}
				if (mode == "revision-changed" || (mode == "revision-during-pull" && pulled)) && args[0] == "list" {
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed","revision":19}]`), nil
				}
				if (mode == "values-changed" || (mode == "values-during-pull" && pulled)) && args[0] == "get" {
					return []byte(`{}`), nil
				}
				if mode == "bad-chart" {
					profile.chartDigest = strings.Repeat("0", 64)
				}
				raw, err := base(ctx, args)
				if mode == "upgrade-failed" && args[0] == "upgrade" {
					return nil, errors.New("private failure canary")
				}
				return raw, err
			}
			err = profile.apply(t.Context(), cfg, deps)
			if err == nil || strings.Contains(err.Error(), "private failure canary") {
				t.Fatal("failed transition accepted or raw error exposed")
			}
			upgrades := 0
			for _, call := range *calls {
				if call[0] == "upgrade" {
					upgrades++
					if call[1] != "argo-workflows" || slices.Contains(call, "--install") || slices.Contains(call, "--create-namespace") {
						t.Fatal("transition broadened resource scope")
					}
				}
			}
			if (mode == "upgrade-failed" || mode == "readback-failed") && upgrades != 1 {
				t.Fatal("did not reach intended mutation boundary")
			}
			if mode != "upgrade-failed" && mode != "readback-failed" && upgrades != 0 {
				t.Fatal("mutation before preconditions")
			}
			if profile.chartPath != "" {
				if _, err := os.Stat(filepath.Dir(profile.chartPath)); !os.IsNotExist(err) {
					t.Fatal("temporary chart retained")
				}
			}
		})
	}
}

func TestUpgradeControllerChartRejectsSymlinkAndWrongDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chart.tgz")
	raw := []byte("public fixture")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyControllerChart(path, digest); err != nil {
		t.Fatal(err)
	}
	if verifyControllerChart(path, strings.Repeat("0", 64)) == nil {
		t.Fatal("wrong digest accepted")
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if verifyControllerChart(link, digest) == nil {
		t.Fatal("symlink chart accepted")
	}
}

func TestUpgradeControllerProfileOverlayIsExactBuiltIn(t *testing.T) {
	cfg, deps, _ := controllerUpgradeFixture(t)
	profile, cleanup, err := prepareUpgradeControllerProfile(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(profile.valuesFile)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(nonrootArgoValues("oberth-argo"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, expected) || profile.chartDigest != "7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50" {
		t.Fatal("profile overlay or chart pin differs")
	}
}

func controllerProfileKube(t *testing.T, namespace string) *fake.Clientset {
	t.Helper()
	raw, err := os.ReadFile("../argojob/testdata/nonroot-argo4.0.8/controller-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
		ConfigMap   corev1.ConfigMap       `json:"configMap"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	created := metav1.NewTime(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	cm := fixture.ConfigMap.DeepCopy()
	cm.Namespace = namespace
	cm.UID = "config-uid"
	cm.ResourceVersion = "1"
	cm.CreationTimestamp = created
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "controller-fixture", Namespace: namespace, UID: "pod-uid", Labels: map[string]string{"app.kubernetes.io/name": "argo-workflows-workflow-controller"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "controller", UID: "replicaset-uid", Controller: ptr.To(true)}}}, Spec: fixture.PodTemplate.Spec}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "kube-api-access-fixture", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0644)), Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
	}}}})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "kube-api-access-fixture", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true, ImageID: argoworkflow.NonrootControllerAMD64, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created}}}}}
	return fake.NewClientset(pod, cm)
}

func TestUpgradeControllerProfileBothReadbacksAndForwardRepair(t *testing.T) {
	cfg, deps, calls := controllerUpgradeFixture(t)
	client := controllerProfileKube(t, "oberth-argo")
	deps.KubeClient = client
	profile, cleanup, err := prepareUpgradeControllerProfile(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sum := sha256.Sum256([]byte("public chart fixture"))
	profile.chartDigest = hex.EncodeToString(sum[:])
	if err := profile.apply(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if profile.verifyFinal(t.Context(), cfg, deps) == nil {
		t.Fatal("Argo-only success mislabeled complete")
	}
	dep := controllerServerFixture(t, client)
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte("oberth v0.17.23"), nil
	}
	if err := profile.verifyFinal(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--argo-controller-profile=other"}, {"--argo-controller-profile=nonroot-static-v1", "--argo-controller-profile=nonroot-static-v1"}} {
		dep.Spec.Template.Spec.Containers[0].Args = args
		if _, err := client.AppsV1().Deployments("oberth").Update(t.Context(), dep, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if profile.verifyFinal(t.Context(), cfg, deps) == nil {
			t.Fatal("missing/foreign/duplicate server profile accepted")
		}
	}
	for _, call := range *calls {
		if call[0] == "rollback" {
			t.Fatal("partial transition attempted rollback")
		}
	}
	// An already-installed exact stronger profile remains valid input for a
	// new supported forward repair, including a failed Argo Helm revision.
	base := deps.RunHelm
	deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "list" && slices.Contains(args, "oberth-argo") {
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"failed","revision":19}]`), nil
		}
		if args[0] == "get" && args[2] == "argo-workflows" {
			return json.Marshal(nonrootArgoValues("oberth-argo"))
		}
		return base(ctx, args)
	}
	next, closeNext, err := prepareUpgradeControllerProfile(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNext()
	next.chartDigest = profile.chartDigest
	if err := next.apply(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeValuePreflightRefusesBeforeControllerMutation(t *testing.T) {
	for _, mode := range []string{"schema", "missing-identity", "helm-too-old"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, calls := controllerUpgradeFixture(t)
			base := deps.RunHelm
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if mode == "helm-too-old" && args[0] == "version" {
					return []byte("v3.13.3"), nil
				}
				if args[0] == "show" {
					return []byte("image:\n  ref: " + controllerServerTestImage + "\n"), nil
				}
				if mode == "missing-identity" && args[0] == "get" && args[2] == "oberth" {
					return []byte(`{"argo":{"namespace":"oberth-argo"}}`), nil
				}
				if mode == "schema" && args[0] == "template" {
					return nil, errors.New("additional properties removedKey not allowed")
				}
				return base(ctx, args)
			}
			result, err := RunUpgrade(t.Context(), cfg, deps)
			if err == nil || result.Upgraded {
				t.Fatalf("preflight failure accepted: %+v %v", result, err)
			}
			for _, call := range *calls {
				if call[0] == "upgrade" || call[0] == "pull" {
					t.Fatalf("controller changed before preflight completed: %v", call)
				}
			}
		})
	}
}
