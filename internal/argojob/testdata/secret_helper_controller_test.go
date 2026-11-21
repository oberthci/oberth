package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
)

func TestOberthSecretHelperPods(t *testing.T) {
	for _, kind := range []string{"container", "script", "container-set", "init", "defaults", "inline"} {
		t.Run(kind, func(t *testing.T) { checkSecretHelperPods(t, kind) })
	}
}

func checkSecretHelperPods(t *testing.T, kind string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("OBERTH_SECRET_HELPER_CASES"), kind+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var w wf.Workflow
	if err = json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	w.Name, w.Namespace = "secret-helper", "default"
	sa := defaultServiceAccount.DeepCopy()
	sa.Name = w.Spec.Executor.ServiceAccountName
	ctx := logging.TestContext(t.Context())
	cancel, c := newController(ctx, &w, sa, defaultServiceAccount, func(c *WorkflowController) {
		c.Config.Executor = c.Config.GetExecutor()
		c.Config.Executor.SecurityContext = secretControllerFloor()
		c.Config.MainContainer = &core.Container{SecurityContext: secretControllerFloor()}
		c.eventRecorderManager = &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(100)}
	})
	defer cancel()
	woc := newWorkflowOperationCtx(ctx, &w, c)
	woc.operate(ctx)
	pods, err := c.kubeclientset.CoreV1().Pods(w.Namespace).List(ctx, meta.ListOptions{})
	if err != nil || len(pods.Items) != 2 {
		t.Fatal("expected private and plain Pods", woc.wf.Status, err)
	}
	raw, err = json.Marshal(pods)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(os.Getenv("OBERTH_SECRET_HELPER_CASES"), kind+".pods.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		declared := strings.HasSuffix(pod.Annotations[common.AnnotationKeyNodeName], ".private")
		checkSecretHelperPod(t, pod, declared)
	}
}

func checkSecretHelperPod(t *testing.T, pod core.Pod, declared bool) {
	t.Helper()
	found := 0
	for _, init := range pod.Spec.InitContainers {
		if init.Name != "oberth-secret-install" {
			continue
		}
		found++
		checkSecretCopyInputs(t, init)
		if !*init.SecurityContext.ReadOnlyRootFilesystem || !*init.SecurityContext.RunAsNonRoot || *pod.Spec.AutomountServiceAccountToken {
			t.Fatal("copy container security or identity differs")
		}
	}
	if (found == 1) != declared || found > 1 {
		t.Fatal("helper added outside declaring Pods", pod.Name, found, declared)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == common.WaitContainerName {
			continue
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == "oberth-secret-bin" {
				mounted = m.ReadOnly && m.MountPath == "/run/oberth/bin"
			}
		}
		if mounted != declared {
			t.Fatal("declaring container lost read-only program mount", c.Name)
		}
	}
}

func checkSecretCopyInputs(t *testing.T, init core.Container) {
	t.Helper()
	// The controller gives all user init containers its public template metadata
	// and runtime emptyDir. Neither is a credential, and the copier reads neither.
	if len(init.EnvFrom) != 0 || len(init.VolumeMounts) != 2 {
		t.Fatal("copy container gained input sources")
	}
	for _, env := range init.Env {
		if !strings.HasPrefix(env.Name, "ARGO_") || env.ValueFrom != nil {
			t.Fatal("copy container gained non-native environment", env.Name)
		}
	}
	for _, mount := range init.VolumeMounts {
		if mount.Name != "oberth-secret-bin" && mount.Name != "var-run-argo" {
			t.Fatal("copy container gained a credential mount", mount.Name)
		}
	}
}

func secretControllerFloor() *core.SecurityContext {
	return &core.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}},
		SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
	}
}
