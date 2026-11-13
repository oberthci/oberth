package installer

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestPlainPKIUpgradeNeedsNoBaoToken(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry-run=%t", dryRun), func(t *testing.T) {
			image := "example.invalid/oberth@sha256:" + strings.Repeat("1", 64)
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace, Generation: 1,
					Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}},
				Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "oberth", Image: image}}},
				}},
				Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
			}
			applied := false
			deps := Deps{
				Output: io.Discard, KubeClient: fake.NewClientset(deployment),
				RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					switch args[0] {
					case "list":
						return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.17.80"}]`), nil
					case "get":
						if !slices.Contains(args, "--all") {
							return []byte(`{"tls":{"pki":{"enabled":true},"extraDNSNames":["ci.example.com"],"extraIPs":["192.0.2.1"]}}`), nil
						}
					case "show":
						return []byte("image:\n  ref: " + image + "\n"), nil
					case "upgrade":
						if slices.Contains(args, "--dry-run=server") {
							return []byte(`{"config":{}}`), nil
						}
						if dryRun || !slices.Contains(args, "--reset-then-reuse-values") || strings.Contains(strings.Join(args, " "), "tls.") {
							t.Fatalf("plain upgrade must reuse live TLS values: %v", args)
						}
						applied = true
						return nil, nil
					}
					if out, ok := testUpgradeValuePreflight(args); ok {
						return out, nil
					}
					return nil, fmt.Errorf("unexpected helm call: %v", args)
				},
				RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
					if !dryRun && applied && name == "kubectl" {
						if slices.Contains(args, "logs") && slices.Contains(args, "--since-time") && slices.Contains(args, "--tail=1000") {
							return nil, nil // bounded migration evidence, no PKI inspection
						}
						if slices.Contains(args, "version") {
							return []byte("oberth v0.17.81\n"), nil
						}
						if slices.Contains(args, "upgrade.completed") {
							return nil, nil
						}
					}
					t.Fatalf("plain upgrade must not inspect PKI: %s %v", name, args)
					return nil, nil
				},
			}
			result, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.17.81", ChartOverride: "fixture", DryRun: dryRun, NoBackup: true}, deps)
			if err != nil || result.Upgraded != !dryRun || applied != !dryRun {
				t.Fatalf("result=%+v applied=%t err=%v", result, applied, err)
			}
		})
	}
}
