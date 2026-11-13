package argojob

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestExecutorTokenWaiterReportsProgressWithoutCredentials(t *testing.T) {
	for _, mode := range []string{"ready", "terminated", "sleep-failed"} {
		t.Run(mode, func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "kubeconfig")
			quoted := "'" + strings.ReplaceAll(token, "'", "'\\''") + "'"
			command := strings.ReplaceAll(executorTokenWaitCommand, executorKubeconfig, quoted)
			prelude := "attempt=0\nsleep() { attempt=$((attempt + 1)); if [ \"$attempt\" -eq 61 ]; then printf '%s' synthetic-token-secret >" + quoted + "; fi; }\n"
			if mode == "terminated" {
				prelude = "sleep() { kill -TERM $$; }\n"
			}
			if mode == "sleep-failed" {
				prelude = "attempt=0\nsleep() { attempt=$((attempt + 1)); if [ \"$attempt\" -eq 2 ]; then printf '%s' synthetic-token-secret >" + quoted + "; fi; return 7; }\n"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "/bin/sh", "-c", prelude+command).CombinedOutput()
			text := string(out)
			if strings.Contains(text, "synthetic-token-secret") || strings.Contains(text, token) {
				t.Fatalf("waiter printed token material or a private path: %s", text)
			}
			if !strings.Contains(text, "waiting for executor token from Oberth") {
				t.Fatalf("waiter gave no initial diagnostic: %v %q", err, text)
			}
			switch mode {
			case "ready":
				if err != nil || strings.Count(text, "still waiting") != 2 || !strings.Contains(text, "30s") || !strings.Contains(text, "60s") || !strings.Contains(text, "executor token ready") {
					t.Fatalf("wait progress/readiness missing or unbounded: %v %q", err, text)
				}
			case "terminated":
				if err == nil || !strings.Contains(text, "executor token wait terminated") {
					t.Fatalf("termination was silent: %v %q", err, text)
				}
			case "sleep-failed":
				if err == nil || !strings.Contains(text, "executor token wait failed") {
					t.Fatalf("failed wait was silent or kept spinning: %v %q", err, text)
				}
			}
		})
	}
}

func TestExecutorTokenWaiterRejectsChangedAndTokenPathCommands(t *testing.T) {
	// The pre-#1564 waiter committed on the in-cluster default token path,
	// which Argo's wait container can be made to read back as an output.
	const tokenPathCommand = "until test -f /var/run/secrets/kubernetes.io/serviceaccount/token; do sleep 1; done"
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes:        []corev1.Volume{{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
		InitContainers: []corev1.Container{executorWaiterContainer("fixed-image", executorWaiterResources(false))},
	}}
	if err := validateExecutorTokenTarget(pod, "fixed-image", false); err != nil {
		t.Fatalf("server-authored waiter rejected: %v", err)
	}
	if strings.Contains(executorTokenWaitCommand, executorTokenDir+"/token") {
		t.Fatal("waiter still commits on the in-cluster default token path")
	}
	for _, command := range []string{tokenPathCommand, executorTokenWaitCommand + "; cat " + executorKubeconfig} {
		pod.Spec.InitContainers[0].Command[2] = command
		if err := validateExecutorTokenTarget(pod, "fixed-image", false); err == nil {
			t.Fatalf("waiter accepted: %q", command)
		}
	}
}
