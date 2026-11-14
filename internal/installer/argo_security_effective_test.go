package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	argoconfig "github.com/argoproj/argo-workflows/v4/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func workflowSeccompValues(podProfile, containerProfile string) map[string]any {
	security := func() map[string]any {
		s := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
		switch containerProfile {
		case "absent":
		case "null":
			s["seccompProfile"] = nil
		default:
			s["seccompProfile"] = map[string]any{"type": containerProfile}
		}
		return s
	}
	controller := map[string]any{}
	if podProfile != "" {
		profile := map[string]any{"type": podProfile}
		if podProfile == "Localhost" {
			profile["localhostProfile"] = "profiles/argo.json"
		}
		controller["workflowDefaults"] = map[string]any{"spec": map[string]any{"securityContext": map[string]any{"seccompProfile": profile}}}
	}
	return map[string]any{"controller": controller, "mainContainer": map[string]any{"securityContext": security()}, "executor": map[string]any{"securityContext": security()}, "server": map[string]any{"enabled": false}}
}

func renderWorkflowSeccomp(t *testing.T, values map[string]any) (string, corev1.Pod) {
	t.Helper()
	flags, _, err := argoSecurityDefaults(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range renderArgoHelmObjects(t, ArgoHelmArgs(Config{argoSecurityArgs: flags}), values) {
		if object["kind"] != "ConfigMap" {
			continue
		}
		raw, ok := valueAt(object, "data", "config").(string)
		if !ok {
			continue
		}
		var native argoconfig.Config
		if err := yaml.Unmarshal([]byte(raw), &native); err != nil {
			t.Fatal(err)
		}
		if native.MainContainer == nil || native.Executor == nil {
			t.Fatal("native chart omitted container defaults")
		}
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"workflows.argoproj.io/workflow": "review"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{*native.MainContainer.DeepCopy(), *native.Executor.DeepCopy()}}}
		pod.Spec.Containers[0].Name, pod.Spec.Containers[1].Name = "main", "wait"
		if native.WorkflowDefaults != nil {
			pod.Spec.SecurityContext = native.WorkflowDefaults.Spec.SecurityContext
		}
		return raw, pod
	}
	t.Fatal("native chart omitted its controller ConfigMap")
	return "", corev1.Pod{}
}

func TestArgoSecurityEffectiveWorkflowSeccomp(t *testing.T) {
	evaluate := argoPodPolicyEvaluator(t)
	for _, tc := range []struct {
		name, storedPod, container, livePod string
		livePass, upgradePass               bool
	}{
		{"pod-unconfined", "Unconfined", "RuntimeDefault", "Unconfined", false, false},
		{"live-pod-unconfined", "RuntimeDefault", "RuntimeDefault", "Unconfined", false, false},
		{"stored-pod-unconfined", "Unconfined", "RuntimeDefault", "RuntimeDefault", true, false},
		{"inherited-runtime-null", "RuntimeDefault", "null", "RuntimeDefault", true, true},
		{"inherited-localhost-null", "Localhost", "null", "Localhost", true, true},
		{"inherited-runtime-absent", "RuntimeDefault", "absent", "RuntimeDefault", true, true},
		{"inherited-localhost-absent", "Localhost", "absent", "Localhost", true, true},
		{"external-inherited", "", "null", "RuntimeDefault", true, true},
		{"container-unconfined", "RuntimeDefault", "Unconfined", "RuntimeDefault", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := workflowSeccompValues(tc.storedPod, tc.container)
			raw, _ := json.Marshal(stored)
			live, pod := renderWorkflowSeccomp(t, workflowSeccompValues(tc.livePod, tc.container))
			failures := evaluate(pod)
			if (len(failures) == 0) != tc.livePass || (!tc.livePass && !strings.Contains(strings.Join(failures, " "), "seccomp")) {
				t.Fatalf("native Pod policy differs: %v", failures)
			}
			if tc.upgradePass && tc.container != "RuntimeDefault" {
				assertInheritedSeccomp(t, stored, pod)
			}
			checkWorkflowSeccompUpgrade(t, raw, live, tc.upgradePass, strings.Contains(tc.name, "pod-unconfined"))
			after, _ := json.Marshal(stored)
			if !bytes.Equal(raw, after) {
				t.Fatal("validation rewrote stored native values")
			}
		})
	}
}

func assertInheritedSeccomp(t *testing.T, stored map[string]any, pod corev1.Pod) {
	t.Helper()
	if pod.Spec.Containers[0].SecurityContext.SeccompProfile != nil || pod.Spec.Containers[1].SecurityContext.SeccompProfile != nil {
		t.Fatal("native chart rewrote inherited container seccomp")
	}
	if valueAt(stored, "controller", "workflowDefaults") == nil {
		return // The live externally configured Pod supplies the inheritance.
	}
	flags, _, err := argoSecurityDefaults(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range flags {
		if strings.HasPrefix(flag, "mainContainer.securityContext.seccompProfile") || strings.HasPrefix(flag, "executor.securityContext.seccompProfile") {
			t.Fatal("native Pod inheritance gained a container override", flag)
		}
	}
}

func checkWorkflowSeccompUpgrade(t *testing.T, stored []byte, live string, pass, podRefusal bool) {
	t.Helper()
	fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
	cm := argoSecurityLiveObjects(DefaultArgoNamespace, true)[1].(*corev1.ConfigMap)
	cm.Data["config"] = live
	if _, err := fixture.deps.KubeClient.CoreV1().ConfigMaps(DefaultArgoNamespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	original := fixture.deps.RunHelm
	policyWrites, nativeWrites := 0, 0
	fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 2 && args[0] == "get" && args[1] == "values" && args[2] == "argo-workflows" {
			return bytes.Clone(stored), nil
		}
		if len(args) > 1 && args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			if args[1] == "argo-workflows" {
				nativeWrites++
			} else {
				policyWrites++
			}
		}
		return original(ctx, args)
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true}, fixture.deps)
	if pass {
		if err != nil || policyWrites != 1 || nativeWrites != 0 {
			t.Fatalf("native inheritance refused or rewritten: policy=%d native=%d err=%v", policyWrites, nativeWrites, err)
		}
	} else if err == nil || policyWrites != 0 || nativeWrites != 0 {
		t.Fatalf("incompatible seccomp reached a policy write: policy=%d native=%d err=%v", policyWrites, nativeWrites, err)
	}
	if podRefusal && (!strings.Contains(err.Error(), "workflowDefaults.spec.securityContext.seccompProfile.type") || !strings.Contains(err.Error(), "Unconfined")) {
		t.Fatalf("Pod refusal omitted its exact value: %v", err)
	}
	current, err := fixture.deps.KubeClient.CoreV1().ConfigMaps(DefaultArgoNamespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
	if err != nil || current.Data["config"] != live {
		t.Fatal("validation rewrote live native configuration", err)
	}
}
