package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const ownershipManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: old-connector
  namespace: core
  labels:
    app.kubernetes.io/component: tunnel-connector
spec: {}
`

func handoffFixture(t *testing.T, manifest string) map[string]any {
	t.Helper()
	sum := sha256.Sum256([]byte(manifest))
	return map[string]any{"format": "oberth-ownership-handoff-v1", "operator": "owner", "verified_at": time.Now().UTC().Format(time.RFC3339Nano), "cluster_uid": "cluster-uid", "source_namespace": "core", "source_revision": "7", "source_manifest_sha256": hex.EncodeToString(sum[:]), "connector": map[string]any{"mode": "external", "owner": "edge-team", "namespace": "edge", "deployment": "connector", "uid": "new-uid", "traffic_verified": true, "credentials_independent": true, "evidence_sha256": strings.Repeat("a", 64)}, "site": map[string]any{"mode": "external", "owner": "docs-team", "repository": "https://forge.example/team/site", "revision": strings.Repeat("b", 40), "url": "https://docs.example", "traffic_verified": true, "credentials_independent": true, "evidence_sha256": strings.Repeat("c", 64)}}
}
func writeHandoff(t *testing.T, r map[string]any) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOwnershipRemovalRequiresFreshIndependentHandoff(t *testing.T) {
	for _, scenario := range []string{"ready", "missing", "stale", "cluster", "manifest", "revision", "connector-unused", "same-namespace", "uid", "not-ready", "paused", "core-owner", "dependency-owner", "missing-dependency", "site-missing", "site-traffic", "credentials", "source-read-failure", "sa-secret", "namespace-owner"} {
		t.Run(scenario, func(t *testing.T) {
			count := int32(1)
			r := handoffFixture(t, ownershipManifest)
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "connector", Namespace: "edge", UID: types.UID("new-uid"), Generation: 2, Labels: map[string]string{"oberth.ci/external-owner": "edge-team"}}, Spec: appsv1.DeploymentSpec{Replicas: &count, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "connector", Volumes: []corev1.Volume{{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}}}}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1}}
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "connector", Namespace: "edge", Labels: map[string]string{"oberth.ci/external-owner": "edge-team"}}}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ca", Namespace: "edge", Labels: map[string]string{"oberth.ci/external-owner": "edge-team"}}}
			switch scenario {
			case "stale":
				r["verified_at"] = time.Now().Add(-25 * time.Hour).Format(time.RFC3339Nano)
			case "cluster":
				r["cluster_uid"] = "elsewhere"
			case "manifest":
				r["source_manifest_sha256"] = strings.Repeat("d", 64)
			case "revision":
				r["source_revision"] = "6"
			case "connector-unused":
				r["connector"].(map[string]any)["mode"] = "unused"
			case "same-namespace":
				r["connector"].(map[string]any)["namespace"] = "core"
			case "uid":
				r["connector"].(map[string]any)["uid"] = "wrong"
			case "not-ready":
				deployment.Status.UpdatedReplicas = 0
			case "paused":
				deployment.Spec.Paused = true
			case "core-owner":
				deployment.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "core"}
			case "dependency-owner":
				cm.Labels = nil
			case "missing-dependency":
				cm.Name = "different"
			case "site-missing":
				delete(r, "site")
			case "site-traffic":
				r["site"].(map[string]any)["traffic_verified"] = false
			case "sa-secret":
				sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "missing-secret"}}
			case "credentials":
				r["connector"].(map[string]any)["credentials_independent"] = false
			}
			externalNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "edge", Labels: map[string]string{"oberth.ci/external-owner": "edge-team"}}}
			if scenario == "namespace-owner" {
				externalNS.Labels = nil
			}
			kube := fake.NewClientset(externalNS, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}}, deployment, sa, cm)
			deps := Deps{KubeClient: kube, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if scenario == "source-read-failure" {
					return nil, fmt.Errorf("unavailable")
				}
				if args[0] == "list" {
					return []byte(`[{"name":"oberth","chart":"oberth-0.17.80","status":"deployed","revision":7}]`), nil
				}
				if args[0] == "get" && args[1] == "manifest" {
					if !strings.Contains(strings.Join(args, " "), "--revision 7") {
						t.Fatal("unpinned manifest")
					}
					return []byte(ownershipManifest), nil
				}
				t.Fatalf("mutation/unexpected Helm command: %v", args)
				return nil, nil
			}}
			path := writeHandoff(t, r)
			if scenario == "missing" {
				path = ""
			}
			err := checkUpgradeOwnership(t.Context(), UpgradeConfig{Namespace: "core", BinaryVersion: "0.17.83", OwnershipHandoff: path}, deps, "0.17.80")
			if scenario == "ready" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "ownership") {
				t.Fatalf("unsafe handoff accepted: %v", err)
			}
			for _, a := range kube.Actions() {
				if a.GetVerb() != "get" {
					t.Fatalf("preflight mutated: %v", a)
				}
			}
		})
	}
}
func TestOwnershipUnusedRequiresEmptySourceAndReason(t *testing.T) {
	r := handoffFixture(t, "")
	r["connector"] = map[string]any{"mode": "unused", "reason": "no connector was installed"}
	r["site"] = map[string]any{"mode": "unused", "reason": "no site is published by this installation"}
	kube := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}})
	deps := Deps{KubeClient: kube, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(`[{"name":"oberth","chart":"oberth-0.17.80","status":"deployed","revision":7}]`), nil
		}
		return []byte{}, nil
	}}
	if err := checkUpgradeOwnership(t.Context(), UpgradeConfig{Namespace: "core", BinaryVersion: "0.17.83", OwnershipHandoff: writeHandoff(t, r)}, deps, "0.17.80"); err != nil {
		t.Fatal(err)
	}
	r["site"].(map[string]any)["reason"] = ""
	if err := checkUpgradeOwnership(t.Context(), UpgradeConfig{Namespace: "core", BinaryVersion: "0.17.83", OwnershipHandoff: writeHandoff(t, r)}, deps, "0.17.80"); err == nil {
		t.Fatal("empty attestation accepted")
	}
}
