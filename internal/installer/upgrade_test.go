package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestValidateUpgradeRejectsDevBuild(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{BinaryVersion: "dev"}
	if err := cfg.ValidateUpgrade(); err == nil {
		t.Fatal("expected error for dev build")
	}
	cfg = UpgradeConfig{BinaryVersion: ""}
	if err := cfg.ValidateUpgrade(); err == nil {
		t.Fatal("expected error for empty version")
	}
}

func TestValidateUpgradeAcceptsReleaseVersion(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{BinaryVersion: "0.12.35"}
	if err := cfg.ValidateUpgrade(); err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Fatalf("timeout = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
}

func TestHelmReleaseRevisionJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, revision, want string
	}{
		{name: "quoted", revision: `"57"`, want: " revision 57"},
		{name: "number", revision: `57`, want: " revision 57"},
		{name: "unknown", revision: `"invalid"`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var releases []helmRelease
			body := `[{"name":"oberth","revision":` + tc.revision + `,"status":"failed"}]`
			if err := json.Unmarshal([]byte(body), &releases); err != nil {
				t.Fatal(err)
			}
			if len(releases) != 1 || helmRevisionLabel(releases[0].Revision) != tc.want {
				t.Fatalf("revision %s decoded as %+v, want label %q", tc.revision, releases, tc.want)
			}
		})
	}
}

func TestRunUpgradeAlreadyUpToDate(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry-run=%t", dryRun), func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			helmCalls := 0
			deps := Deps{
				Output: &output,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					helmCalls++
					if len(args) >= 2 && args[0] == "list" {
						return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.12.35"}]`), nil
					}
					return nil, fmt.Errorf("unexpected helm call: %v", args)
				},
				KubeClient: fake.NewClientset(),
				RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
			}

			result, err := RunUpgrade(context.Background(), UpgradeConfig{
				BinaryVersion: "0.12.35", Namespace: "oberth", DryRun: dryRun,
			}, deps)
			if err != nil {
				t.Fatal(err)
			}
			if !result.AlreadyUpToDate || result.Upgraded || helmCalls != 1 ||
				!strings.Contains(output.String(), "Already running v0.12.35") {
				t.Fatalf("deployed same-version result=%+v, helmCalls=%d, output=%q", result, helmCalls, output.String())
			}
		})
	}
}

func TestRunUpgradeFailedSameVersionDryRunPlansRepair(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	helmCalls := 0
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCalls++
			if len(args) >= 2 && args[0] == "list" {
				return []byte(`[{"name":"oberth","namespace":"oberth","revision":"57","status":"failed","chart":"oberth-0.16.6"}]`), nil
			}
			return nil, fmt.Errorf("unexpected Helm mutation in dry-run: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}
	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.16.6", Namespace: "oberth", DryRun: true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"revision 57 is FAILED", "same-version forward repair", "helm upgrade oberth", "--version 0.16.6", "No cluster changes were made"} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry-run output missing %q: %q", want, text)
		}
	}
	if result.AlreadyUpToDate || result.Upgraded || helmCalls != 1 || strings.Contains(text, "Already running") {
		t.Fatalf("failed release dry-run result=%+v, helmCalls=%d, output=%q", result, helmCalls, text)
	}
}

func TestRunUpgradeSameVersionRefusesPendingAndUnknownStates(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"pending-install", "pending-upgrade", "pending-rollback", "", "unknown"} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%q/dry-run=%t", status, dryRun), func(t *testing.T) {
				t.Parallel()
				var output bytes.Buffer
				helmCalls := 0
				deps := Deps{
					Output: &output,
					RunHelm: func(_ context.Context, args []string) ([]byte, error) {
						helmCalls++
						if len(args) >= 2 && args[0] == "list" {
							data, _ := json.Marshal([]helmRelease{{Name: "oberth", Namespace: "oberth", Status: status, Chart: "oberth-0.16.6"}})
							return data, nil
						}
						return nil, fmt.Errorf("unexpected helm call: %v", args)
					},
					KubeClient: fake.NewClientset(),
					RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
				}
				result, err := RunUpgrade(context.Background(), UpgradeConfig{
					BinaryVersion: "0.16.6", Namespace: "oberth", DryRun: dryRun,
				}, deps)
				if err == nil || !strings.Contains(err.Error(), "helm status oberth -n oberth") {
					t.Fatalf("non-deployed same-version state %q error = %v", status, err)
				}
				if result.AlreadyUpToDate || result.Upgraded || helmCalls != 1 || strings.Contains(output.String(), "Already running") {
					t.Fatalf("false success for %q: result=%+v, helmCalls=%d, output=%q", status, result, helmCalls, output.String())
				}
			})
		}
	}
}

func TestRunUpgradeRetriesFailedSameVersionWithPinnedImage(t *testing.T) {
	t.Parallel()
	const targetVersion = "0.16.6"
	const targetImage = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth", Namespace: "oberth", Generation: 2,
			Labels: map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
	var output bytes.Buffer
	var calls [][]string
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"failed","chart":"oberth-0.16.6"}]`), nil
			case "repo":
				return nil, nil
			case "show":
				return []byte("image:\n  ref: " + targetImage + "\n"), nil
			case "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("oberth v0.16.6 commit=abc123 date=2026-09-28T00:00:00Z\n"), nil
		},
		KubeClient: fake.NewClientset(deployment),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}
	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: targetVersion, Namespace: "oberth", Timeout: 10 * time.Second,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyUpToDate || !strings.Contains(output.String(), "FAILED") || strings.Contains(output.String(), "Already running") {
		t.Fatalf("failed same-version retry result=%+v, output=%q", result, output.String())
	}
	if len(calls) != 4 || calls[0][0] != "list" || calls[1][0] != "repo" || calls[2][0] != "show" || calls[3][0] != "upgrade" {
		t.Fatalf("unexpected Helm flow: %v", calls)
	}
	show := strings.Join(calls[2], " ")
	upgrade := strings.Join(calls[3], " ")
	if !strings.Contains(show, "--version "+targetVersion) ||
		!strings.Contains(upgrade, "--version "+targetVersion) ||
		!strings.Contains(upgrade, "image.ref="+targetImage) ||
		!strings.Contains(upgrade, "--wait") {
		t.Fatalf("retry lost version/image pin: show=%q upgrade=%q", show, upgrade)
	}
}

func TestRunUpgradeFailedSameVersionRejectsTagOnlyImage(t *testing.T) {
	t.Parallel()
	var upgradeCalls int
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"failed","chart":"oberth-0.16.6"}]`), nil
			case "repo":
				return nil, nil
			case "show":
				return []byte("image:\n  ref: europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.16.6\n"), nil
			case "upgrade":
				upgradeCalls++
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}
	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.16.6", Namespace: "oberth",
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") || result.Upgraded || result.AlreadyUpToDate || upgradeCalls != 0 {
		t.Fatalf("tag-only same-version retry: result=%+v, err=%v, upgradeCalls=%d", result, err, upgradeCalls)
	}
}

func TestRunUpgradeNoRelease(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				return []byte("[]"), nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "no Oberth Helm release found") {
		t.Fatalf("error = %v, want 'no Oberth Helm release found'", err)
	}
}

func TestRunUpgradeRejectsDowngrade(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"deployed", "failed"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			helmCalls := 0
			deps := Deps{
				Output: io.Discard,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					helmCalls++
					if len(args) >= 2 && args[0] == "list" {
						data, _ := json.Marshal([]helmRelease{{
							Name: "oberth", Namespace: "oberth", Status: status, Chart: "oberth-0.12.35",
						}})
						return data, nil
					}
					return nil, fmt.Errorf("unexpected helm call: %v", args)
				},
				KubeClient: fake.NewClientset(),
				RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
			}
			_, err := RunUpgrade(context.Background(), UpgradeConfig{
				BinaryVersion: "0.12.34", Namespace: "oberth",
			}, deps)
			if err == nil || !strings.Contains(err.Error(), "newer than CLI version") || helmCalls != 1 {
				t.Fatalf("status=%q downgrade error=%v, helmCalls=%d", status, err, helmCalls)
			}
		})
	}
}

func TestRunUpgradeRejectsUnparseableInstalledVersion(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name:      "oberth",
					Namespace: "oberth",
					Status:    "deployed",
					Chart:     "oberth-garbage",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "could not be parsed") {
		t.Fatalf("unparseable error = %v", err)
	}
}

func TestRunUpgradeDryRun(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name:      "oberth",
					Namespace: "oberth",
					Status:    "deployed",
					Chart:     "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		DryRun:        true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousVersion != "0.12.34" || result.TargetVersion != "0.12.35" {
		t.Fatalf("result = %+v", result)
	}
	text := output.String()
	if !strings.Contains(text, "Dry-run plan") {
		t.Fatalf("output = %q", text)
	}
	if !strings.Contains(text, "v0.12.34") || !strings.Contains(text, "v0.12.35") {
		t.Fatalf("output = %q", text)
	}
	if !strings.Contains(text, "No cluster changes were made") {
		t.Fatalf("output = %q", text)
	}
}

func TestUpgradeHelmArgs(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.12.35",
		Timeout:       5 * time.Minute,
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "upgrade oberth oberth-charts/oberth") {
		t.Fatalf("args = %q", joined)
	}
	if !strings.Contains(joined, "--reuse-values") {
		t.Fatalf("missing --reuse-values: %q", joined)
	}
	if !strings.Contains(joined, "--set-string image.ref=") {
		t.Fatalf("missing --set-string image.ref: %q", joined)
	}
	if !strings.Contains(joined, "--version 0.12.35") {
		t.Fatalf("missing --version: %q", joined)
	}
	if !strings.Contains(joined, "--timeout 5m0s") {
		t.Fatalf("missing --timeout: %q", joined)
	}
	if !strings.Contains(joined, "--wait") {
		t.Fatalf("missing --wait: %q", joined)
	}
	// Go module proxy pins: --reuse-values discards new chart defaults,
	// so the upgrade path must pin them explicitly (#662).
	if strings.Contains(joined, "argo.goProxy.enabled=") {
		t.Fatalf("upgrade overwrites the existing proxy enablement: %q", joined)
	}
	if !strings.Contains(joined, "--set argo.goProxy.port=8444") {
		t.Fatalf("missing argo.goProxy.port pin: %q", joined)
	}
}

func TestUpgradeHelmArgsPinsGoProxyBeforeVersion(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.16.20",
		Timeout:       5 * time.Minute,
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")

	// The goProxy pins must appear between --set-string image.ref and
	// --version so --reuse-values processes them before the chart selection.
	var imageIdx, portIdx, versionIdx int
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "image.ref="):
			imageIdx = i
		case arg == "argo.goProxy.port=8444":
			portIdx = i
		case arg == "0.16.20" && i > 0 && args[i-1] == "--version":
			versionIdx = i
		}
	}
	if portIdx == 0 {
		t.Fatalf("goProxy pins not found in args: %v", args)
	}
	if portIdx <= imageIdx {
		t.Fatalf("goProxy.port (idx %d) must come after image.ref (idx %d)", portIdx, imageIdx)
	}
	if versionIdx <= portIdx {
		t.Fatalf("--version (idx %d) must come after goProxy.port (idx %d)", versionIdx, portIdx)
	}
}

func TestUpgradeHelmArgsLocalChart(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.12.35",
		ChartOverride: "/tmp/chart",
	}
	args := upgradeHelmArgs(cfg, "/tmp/chart", "oberth:dev")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--version") {
		t.Fatalf("local chart should not have --version: %q", joined)
	}
}

func TestDeploymentRolledOut(t *testing.T) {
	t.Parallel()
	replicas := int32(1)
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35"

	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		want       bool
	}{
		{
			name: "fully rolled out",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     1,
					AvailableReplicas:   1,
					UnavailableReplicas: 0,
				},
			},
			want: true,
		},
		{
			name: "wrong image",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "oberth", Image: "old-image"}},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     1,
					AvailableReplicas:   1,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "generation not observed",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     1,
					AvailableReplicas:   1,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "unavailable replicas",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     1,
					AvailableReplicas:   0,
					UnavailableReplicas: 1,
				},
			},
			want: false,
		},
		{
			name: "nil replicas",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2,
				},
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := deploymentRolledOut(test.deployment, targetImage)
			if got != test.want {
				t.Fatalf("deploymentRolledOut = %v, want %v", got, test.want)
			}
		})
	}
}

func TestVerifyRunningVersionParsesOutput(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("oberth v0.12.35 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
	}
	version, err := verifyRunningVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"}, "0.12.35")
	if err != nil {
		t.Fatal(err)
	}
	if version != "v0.12.35" {
		t.Fatalf("version = %q, want v0.12.35", version)
	}
}

func TestVerifyRunningVersionHandlesKubectlNoise(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("Defaulted container \"oberth\" out of: oberth, init\noberth v0.12.35 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
	}
	version, err := verifyRunningVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"}, "0.12.35")
	if err != nil {
		t.Fatal(err)
	}
	if version != "v0.12.35" {
		t.Fatalf("version = %q, want v0.12.35", version)
	}
}

func TestVerifyRunningVersionRejectsMismatch(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("oberth v0.12.20 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
	}
	_, err := verifyRunningVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"}, "0.12.35")
	if err == nil || !strings.Contains(err.Error(), "expected v0.12.35 but pod reports v0.12.20") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestVerifyRunningVersionRejectsUnparseable(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("oberth dev commit=unknown date=unknown\n"), nil
		},
	}
	_, err := verifyRunningVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"}, "0.12.35")
	if err == nil || !strings.Contains(err.Error(), "could not parse") {
		t.Fatalf("unparseable error = %v", err)
	}
}

func TestResolveChartImageRef(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	ref, _, err := resolveChartImageRef(context.Background(), deps, "oberth-charts/oberth", "0.12.35", false)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("ref = %q", ref)
	}
}

func TestResolveChartImageRefEmpty(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: \"\"\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	_, _, err := resolveChartImageRef(context.Background(), deps, "oberth-charts/oberth", "0.12.35", false)
	if err == nil || !strings.Contains(err.Error(), "chart does not define image.ref") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunUpgradeFullFlow(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "oberth",
			Namespace:  "oberth",
			Generation: 2,
			Labels:     map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}

	kubeClient := fake.NewClientset(deployment)

	helmCallLog := make([]string, 0)
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCallLog = append(helmCallLog, args[0])
			switch {
			case args[0] == "list":
				releases := []helmRelease{{
					Name:      "oberth",
					Namespace: "oberth",
					Status:    "deployed",
					Chart:     "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case args[0] == "repo":
				return nil, nil
			case args[0] == "show" && args[1] == "values":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case args[0] == "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("oberth v0.12.35 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
		KubeClient: kubeClient,
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       10 * time.Second,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded {
		t.Fatal("expected Upgraded")
	}
	if result.PreviousVersion != "0.12.34" {
		t.Fatalf("PreviousVersion = %q", result.PreviousVersion)
	}
	text := output.String()
	if !strings.Contains(text, "v0.12.34") || !strings.Contains(text, "v0.12.35") {
		t.Fatalf("output = %q", text)
	}
	if !strings.Contains(text, "ready") {
		t.Fatalf("output = %q", text)
	}
	if !strings.Contains(text, "Target:") {
		t.Fatalf("output should contain target disclosure: %q", text)
	}
}

// --- #70: digest-form validation ---

func TestResolveChartImageRefRejectsTagOnly(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	_, _, err := resolveChartImageRef(context.Background(), deps, "oberth-charts/oberth", "0.12.35", false)
	if err == nil || !strings.Contains(err.Error(), "not in digest-pinned form") {
		t.Fatalf("tag-only ref error = %v", err)
	}
}

func TestResolveChartImageRefRejectsShortDigest(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:abc123\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	_, _, err := resolveChartImageRef(context.Background(), deps, "oberth-charts/oberth", "0.12.35", false)
	if err == nil || !strings.Contains(err.Error(), "not in digest-pinned form") {
		t.Fatalf("short digest ref error = %v", err)
	}
}

func TestResolveChartImageRefRejectsNonGARRegistry(t *testing.T) {
	t.Parallel()
	validDigest := "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "show" && args[1] == "values" {
				return []byte(fmt.Sprintf("image:\n  ref: other-registry.example.com/repo/oberth@%s\n", validDigest)), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	_, _, err := resolveChartImageRef(context.Background(), deps, "oberth-charts/oberth", "0.12.35", false)
	if err == nil || !strings.Contains(err.Error(), "canonical GAR prefix") {
		t.Fatalf("non-GAR registry error = %v", err)
	}
}

func TestResolveChartImageRefLocalChartSkipsRegistryCheck(t *testing.T) {
	t.Parallel()
	validDigest := "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "show" && args[1] == "values" {
				return []byte(fmt.Sprintf("image:\n  ref: my-local-registry.dev/oberth@%s\n", validDigest)), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	ref, _, err := resolveChartImageRef(context.Background(), deps, "/tmp/chart", "0.12.35", true)
	if err != nil {
		t.Fatalf("local chart with non-GAR registry should pass: %v", err)
	}
	if !strings.Contains(ref, validDigest) {
		t.Fatalf("ref = %q", ref)
	}
}

func TestResolveChartImageRefLocalChartStillRequiresDigest(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: my-local-registry.dev/oberth:latest\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	_, _, err := resolveChartImageRef(context.Background(), deps, "/tmp/chart", "0.12.35", true)
	if err == nil || !strings.Contains(err.Error(), "not in digest-pinned form") {
		t.Fatalf("local chart without digest error = %v", err)
	}
}

// --- #71: non-local guard ---

func TestRunUpgradeRejectsNonLocalWithoutYes(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output:      &output,
		ContextName: "prod-cluster",
		RestConfig:  &rest.Config{Host: "https://example.com:6443"},
		KubeClient:  fake.NewClientset(),
	}
	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "does not appear to be a local cluster") {
		t.Fatalf("non-local guard error = %v", err)
	}
	if !strings.Contains(output.String(), "Target: prod-cluster") {
		t.Fatalf("output should contain target disclosure: %q", output.String())
	}
}

func TestRunUpgradeAllowsNonLocalWithYes(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output:      &output,
		ContextName: "prod-cluster",
		RestConfig:  &rest.Config{Host: "https://example.com:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("[]"), nil // no releases — proves we passed the guard
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
		KubeClient: fake.NewClientset(),
	}
	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Yes:           true,
	}, deps)
	// Should get past the non-local guard and fail on "no release found".
	if err == nil || !strings.Contains(err.Error(), "no Oberth Helm release found") {
		t.Fatalf("expected 'no release found' (proving guard was bypassed), got: %v", err)
	}
	if !strings.Contains(output.String(), "WARNING:") {
		t.Fatalf("non-local --yes should print WARNING: %q", output.String())
	}
}

// --- #72: failure path gaps ---

func TestUpgradeHelmArgsNoAtomic(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.12.35",
		Timeout:       5 * time.Minute,
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "some-image@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	for _, arg := range args {
		if arg == "--atomic" {
			t.Fatal("--atomic must not be present in upgrade helm args: auto-rollback + forward-only migrations = crashloop trap")
		}
	}
}

func TestUpgradeHelmArgsPassesTimeout(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.12.35",
		Timeout:       3 * time.Minute,
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "image@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--timeout 3m0s") {
		t.Fatalf("missing --timeout 3m0s: %q", joined)
	}
}

func TestRunUpgradeRejectsEmptyInstalledVersion(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name:      "oberth",
					Namespace: "oberth",
					Status:    "deployed",
					Chart:     "oberth", // no version suffix
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
	}, deps)
	if err == nil {
		t.Fatal("expected error for empty installed version")
	}
	if !strings.Contains(err.Error(), "no version could be determined") {
		t.Fatalf("error = %v, want 'no version could be determined'", err)
	}
	if result.Upgraded {
		t.Fatal("should not be upgraded")
	}
}

func TestRunUpgradeRetriesTransientVerifyExecFailure(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "oberth",
			Namespace:  "oberth",
			Generation: 2,
			Labels:     map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}

	kubeClient := fake.NewClientset(deployment)

	execAttempts := 0
	deps := Deps{
		Output:       &output,
		PollInterval: time.Millisecond,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case args[0] == "list":
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case args[0] == "repo":
				return nil, nil
			case args[0] == "show" && args[1] == "values":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case args[0] == "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			execAttempts++
			if execAttempts < 2 {
				return nil, fmt.Errorf("error: unable to upgrade connection: container not found")
			}
			return []byte("oberth v0.12.35 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
		KubeClient: kubeClient,
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	result, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       10 * time.Second,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded {
		t.Fatal("expected Upgraded")
	}
	if execAttempts < 2 {
		t.Fatalf("expected at least 2 exec attempts, got %d", execAttempts)
	}
}

func TestRunUpgradeFailsWhenVersionUnverifiable(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "oberth",
			Namespace:  "oberth",
			Generation: 2,
			Labels:     map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}

	kubeClient := fake.NewClientset(deployment)

	execAttempts := 0
	deps := Deps{
		Output:       &output,
		PollInterval: time.Millisecond,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case args[0] == "list":
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case args[0] == "repo":
				return nil, nil
			case args[0] == "show" && args[1] == "values":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case args[0] == "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			execAttempts++
			return nil, fmt.Errorf("error: unable to upgrade connection: container not found")
		},
		KubeClient: kubeClient,
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       10 * time.Second,
	}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "verify manually") {
		t.Fatalf("error should contain 'verify manually': %v", err)
	}
	if execAttempts != verifyMaxAttempts {
		t.Fatalf("expected exactly %d exec attempts, got %d", verifyMaxAttempts, execAttempts)
	}
	// Recovery guidance should be printed on version-verify failure.
	text := output.String()
	if !strings.Contains(text, "DO NOT run 'helm rollback'") {
		t.Fatalf("missing recovery guidance in output: %q", text)
	}
}

func TestRunUpgradeFailsOnVersionMismatchFullFlow(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:v0.12.35@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "oberth",
			Namespace:  "oberth",
			Generation: 2,
			Labels:     map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "oberth", Image: targetImage}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}

	kubeClient := fake.NewClientset(deployment)

	execAttempts := 0
	deps := Deps{
		Output:       &output,
		PollInterval: time.Millisecond,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case args[0] == "list":
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case args[0] == "repo":
				return nil, nil
			case args[0] == "show" && args[1] == "values":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case args[0] == "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			execAttempts++
			return []byte("oberth v0.12.20 commit=abc123 date=2026-08-18T00:00:00Z\n"), nil
		},
		KubeClient: kubeClient,
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       10 * time.Second,
	}, deps)
	if err == nil {
		t.Fatal("expected error for version mismatch")
	}
	if !strings.Contains(err.Error(), "v0.12.35") || !strings.Contains(err.Error(), "v0.12.20") {
		t.Fatalf("error should name both versions: %v", err)
	}
	// Version mismatch is definitive — must NOT retry.
	if execAttempts != 1 {
		t.Fatalf("expected exactly 1 exec attempt (no retry on mismatch), got %d", execAttempts)
	}
}

func TestRunUpgradeRecoveryGuidanceOnHelmFailure(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	validDigest := "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@" + validDigest
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				releases := []helmRelease{{
					Name:      "oberth",
					Namespace: "oberth",
					Status:    "deployed",
					Chart:     "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case "repo":
				return nil, nil
			case "show":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case "upgrade":
				return nil, fmt.Errorf("helm upgrade failed: release timed out")
			default:
				return nil, fmt.Errorf("unexpected: %v", args)
			}
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       10 * time.Second,
	}, deps)
	if err == nil {
		t.Fatal("expected error from helm upgrade failure")
	}
	text := output.String()
	if !strings.Contains(text, "DO NOT run 'helm rollback'") {
		t.Fatalf("missing recovery guidance in output: %q", text)
	}
	if !strings.Contains(text, "forward-only") {
		t.Fatalf("missing forward-only mention in output: %q", text)
	}
	if !strings.Contains(text, "helm status oberth") {
		t.Fatalf("missing helm status pointer in output: %q", text)
	}
	if !strings.Contains(text, "kubectl logs") {
		t.Fatalf("missing kubectl logs pointer in output: %q", text)
	}
}

func TestRunUpgradeRecoveryGuidanceOnRolloutFailure(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	validDigest := "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	targetImage := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@" + validDigest
	replicas := int32(1)

	// Deployment with wrong image so rollout never completes.
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "oberth",
			Namespace:  "oberth",
			Generation: 2,
			Labels:     map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "oberth", Image: "old-image"}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			UpdatedReplicas:     0,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}

	kubeClient := fake.NewClientset(deployment)

	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case args[0] == "list":
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			case args[0] == "repo":
				return nil, nil
			case args[0] == "show" && args[1] == "values":
				return []byte(fmt.Sprintf("image:\n  ref: %s\n", targetImage)), nil
			case args[0] == "upgrade":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected helm call: %v", args)
			}
		},
		KubeClient: kubeClient,
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	// Use a very short timeout so the rollout times out quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := RunUpgrade(ctx, UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		Timeout:       100 * time.Millisecond,
	}, deps)
	if err == nil {
		t.Fatal("expected error from rollout timeout")
	}
	text := output.String()
	if !strings.Contains(text, "DO NOT run 'helm rollback'") {
		t.Fatalf("missing recovery guidance in output: %q", text)
	}
	if !strings.Contains(text, "helm status oberth") {
		t.Fatalf("missing helm status pointer in output: %q", text)
	}
	if !strings.Contains(text, "kubectl logs") {
		t.Fatalf("missing kubectl logs pointer in output: %q", text)
	}
}

// --- #704: --values/--set, pinned-key protection, schema validation, dry-run single source of truth ---

func TestCheckPinnedKeyConflictsRefusesImageRef(t *testing.T) {
	t.Parallel()
	err := checkPinnedKeyConflicts([]string{"image.ref=custom:latest"}, nil)
	var pinned *ErrPinnedKeyOverride
	if !errors.As(err, &pinned) || pinned.Key != "image.ref" {
		t.Fatalf("expected ErrPinnedKeyOverride for image.ref, got %v", err)
	}
}

func TestCheckPinnedKeyConflictsRefusesGoProxy(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"argo.goProxy.enabled=false", "argo.goProxy.port=9999"} {
		err := checkPinnedKeyConflicts([]string{key}, nil)
		var pinned *ErrPinnedKeyOverride
		if !errors.As(err, &pinned) {
			t.Fatalf("expected ErrPinnedKeyOverride for %q, got %v", key, err)
		}
	}
}

func TestCheckPinnedKeyConflictsRefusesParentKey(t *testing.T) {
	t.Parallel()
	// Setting "image" would wipe "image.ref".
	err := checkPinnedKeyConflicts([]string{"image=null"}, nil)
	var pinned *ErrPinnedKeyOverride
	if !errors.As(err, &pinned) {
		t.Fatalf("expected ErrPinnedKeyOverride for parent key, got %v", err)
	}
}

func TestCheckPinnedKeyConflictsAllowsValidKey(t *testing.T) {
	t.Parallel()
	err := checkPinnedKeyConflicts([]string{"maxConcurrentJobs=5"}, nil)
	if err != nil {
		t.Fatalf("expected no error for valid key, got %v", err)
	}
}

func TestCheckPinnedKeyConflictsInValuesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/vals.yaml"
	if err := os.WriteFile(path, []byte("image:\n  ref: custom:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkPinnedKeyConflicts(nil, []string{path})
	var pinned *ErrPinnedKeyOverride
	if !errors.As(err, &pinned) || pinned.Key != "image.ref" {
		t.Fatalf("expected ErrPinnedKeyOverride for values file, got %v", err)
	}
}

func TestCheckValuesPinnedConflictsAllowsNonPinnedKeys(t *testing.T) {
	t.Parallel()
	data := []byte("maxConcurrentJobs: 5\npushBannerURL: https://example.com\n")
	if err := checkValuesPinnedConflicts(data); err != nil {
		t.Fatalf("expected no error for non-pinned keys, got %v", err)
	}
}

func TestValidateUserSetAgainstValuesRefusesUnknownKey(t *testing.T) {
	t.Parallel()
	valuesYAML := []byte("maxConcurrentJobs: 3\nimage:\n  ref: x\n")
	err := validateUserSetAgainstValues(valuesYAML, []string{"nonexistent.key=42"})
	var schema *ErrSchemaValidation
	if !errors.As(err, &schema) || !strings.Contains(schema.Detail, "nonexistent.key") {
		t.Fatalf("expected ErrSchemaValidation for unknown key, got %v", err)
	}
}

func TestValidateUserSetAgainstValuesAllowsKnownKey(t *testing.T) {
	t.Parallel()
	valuesYAML := []byte("maxConcurrentJobs: 3\nimage:\n  ref: x\n")
	err := validateUserSetAgainstValues(valuesYAML, []string{"maxConcurrentJobs=5"})
	if err != nil {
		t.Fatalf("expected no error for known key, got %v", err)
	}
}

func TestUpgradeHelmArgsIncludesUserValues(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.16.20",
		Timeout:       5 * time.Minute,
		UserValues:    []string{"/tmp/custom.yaml"},
		UserSet:       []string{"maxConcurrentJobs=5"},
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "img@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--values /tmp/custom.yaml") {
		t.Fatalf("missing --values in args: %q", joined)
	}
	if !strings.Contains(joined, "--set maxConcurrentJobs=5") {
		t.Fatalf("missing --set in args: %q", joined)
	}
}

func TestUpgradeHelmArgsUserValuesAfterPins(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.16.20",
		Timeout:       5 * time.Minute,
		UserValues:    []string{"/tmp/custom.yaml"},
		UserSet:       []string{"maxConcurrentJobs=5"},
	}
	args := upgradeHelmArgs(cfg, "oberth-charts/oberth", "img@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	var goProxyIdx, valuesIdx, userSetIdx int
	for i, arg := range args {
		switch {
		case arg == "argo.goProxy.port=8444":
			goProxyIdx = i
		case arg == "/tmp/custom.yaml" && i > 0 && args[i-1] == "--values":
			valuesIdx = i
		case arg == "maxConcurrentJobs=5" && i > 0 && args[i-1] == "--set":
			userSetIdx = i
		}
	}
	if valuesIdx == 0 || userSetIdx == 0 || goProxyIdx == 0 {
		t.Fatalf("missing expected args: goProxy=%d values=%d set=%d in %v", goProxyIdx, valuesIdx, userSetIdx, args)
	}
	if valuesIdx <= goProxyIdx {
		t.Fatalf("user --values (idx %d) must come after goProxy pin (idx %d)", valuesIdx, goProxyIdx)
	}
	if userSetIdx <= valuesIdx {
		t.Fatalf("user --set (idx %d) must come after --values (idx %d)", userSetIdx, valuesIdx)
	}
}

func TestDryRunRendersGoProxyPins(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.12.34",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.12.35",
		Namespace:     "oberth",
		DryRun:        true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "argo.goProxy.enabled=") {
		t.Fatalf("dry-run output unexpected goProxy.enabled override: %q", text)
	}
	if !strings.Contains(text, "argo.goProxy.port=8444") {
		t.Fatalf("dry-run output missing goProxy.port pin: %q", text)
	}
}

func TestDryRunIncludesUserValues(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.16.18",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion: "0.16.19",
		Namespace:     "oberth",
		DryRun:        true,
		UserSet:       []string{"maxConcurrentJobs=5"},
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "maxConcurrentJobs=5") {
		t.Fatalf("dry-run output missing user --set value: %q", text)
	}
}

func TestDryRunAndRealArgsAgree(t *testing.T) {
	t.Parallel()
	cfg := UpgradeConfig{
		Namespace:     "oberth",
		BinaryVersion: "0.16.20",
		Timeout:       5 * time.Minute,
		UserSet:       []string{"maxConcurrentJobs=5"},
	}
	realArgs := upgradeHelmArgs(cfg, "oberth-charts/oberth", "img@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	dryArgs := upgradeHelmArgs(cfg, "oberth-charts/oberth", "<chart-default>")

	if len(realArgs) != len(dryArgs) {
		t.Fatalf("arg count mismatch: real=%d dry=%d\nreal=%v\ndry=%v", len(realArgs), len(dryArgs), realArgs, dryArgs)
	}
	for i := range realArgs {
		if realArgs[i] != dryArgs[i] {
			if !strings.HasPrefix(realArgs[i], "image.ref=") && !strings.HasPrefix(dryArgs[i], "image.ref=") {
				t.Fatalf("args disagree at position %d: real=%q dry=%q", i, realArgs[i], dryArgs[i])
			}
		}
	}
}

// --- #711: schema migration announcement ---

func TestQueryRunningSchemaVersionParsesOutput(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			// Verify the command calls version --schema, not just version.
			for i, arg := range args {
				if arg == "version" && i+1 < len(args) && args[i+1] == "--schema" {
					return []byte("17\n"), nil
				}
			}
			return nil, fmt.Errorf("unexpected args: %v", args)
		},
	}
	v, err := queryRunningSchemaVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"})
	if err != nil {
		t.Fatal(err)
	}
	if v != 17 {
		t.Fatalf("schema version = %d, want 17", v)
	}
}

func TestQueryRunningSchemaVersionReturnsZeroForOldBinary(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output: io.Discard,
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			// Old binary does not understand --schema, returns usage error.
			return []byte("oberth: usage error: version accepts no arguments\n"), fmt.Errorf("exit status 2")
		},
	}
	v, err := queryRunningSchemaVersion(context.Background(), deps, UpgradeConfig{Namespace: "oberth"})
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("schema version = %d, want 0 for old binary", v)
	}
}

func TestUpgradeAnnouncesSchemaWhenMigrationPending(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.16.18",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("16\n"), nil
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion:       "0.16.19",
		Namespace:           "oberth",
		DryRun:              true,
		BinarySchemaVersion: 17,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "Schema migration v17 will run") {
		t.Fatalf("expected schema migration announcement, got: %q", text)
	}
	if !strings.Contains(text, "forward-only") {
		t.Fatalf("expected forward-only warning in announcement, got: %q", text)
	}
}

func TestUpgradeRequiresYesForNonLocalSchemaChange(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output:      &output,
		ContextName: "prod-cluster",
		RestConfig:  &rest.Config{Host: "https://example.com:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.16.18",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("16\n"), nil
		},
		KubeClient: fake.NewClientset(),
	}

	// Non-local without --yes should be caught by the existing guard
	// BEFORE schema migration. The schema migration check provides an
	// additional layer for the case where --yes is not set.
	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion:       "0.16.19",
		Namespace:           "oberth",
		Yes:                 false,
		BinarySchemaVersion: 17,
	}, deps)
	if err == nil {
		t.Fatal("expected error for non-local without --yes")
	}
}

func TestUpgradeSilentWhenSchemaUnchanged(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "list" {
				releases := []helmRelease{{
					Name: "oberth", Namespace: "oberth",
					Status: "deployed", Chart: "oberth-0.16.18",
				}}
				data, _ := json.Marshal(releases)
				return data, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			return []byte("17\n"), nil
		},
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
	}

	_, err := RunUpgrade(context.Background(), UpgradeConfig{
		BinaryVersion:       "0.16.19",
		Namespace:           "oberth",
		DryRun:              true,
		BinarySchemaVersion: 17,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "Schema migration") {
		t.Fatalf("should not announce schema migration when version unchanged: %q", text)
	}
}
