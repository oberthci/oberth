package argojob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Argo's controller deletes a fulfilled Workflow's task results with a
// DeleteCollection under its own token, which is no executor holder. The
// task-result policy must let that native cleanup through.
func TestLiveArgoTaskResultCleanupUnderPolicy(t *testing.T) {
	cluster := requireLiveCluster(t)
	cluster.ensureServiceAccounts(t, t.Context())
	installLiveTaskResultPolicy(t, cluster)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	since := metav1.Now()
	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatal(err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	name := fmt.Sprintf("r7-cleanup-%d", time.Now().Unix())
	request := liveRequest(name, pipeline.TriggerCI, liveTwoStageDocument)
	request.SourceDir = writeCheckout(t)
	t.Cleanup(func() {
		_ = cluster.workflows.Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	completion, err := controller.Wait(ctx, name, request.RunID, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !completion.Succeeded {
		t.Fatalf("Workflow did not succeed: phase=%s reason=%s", completion.Phase, completion.Reason)
	}
	// Real holders match the structural holder check, so settling retired them.
	holders, err := cluster.kube.CoreV1().Pods(cluster.namespace).List(ctx, metav1.ListOptions{LabelSelector: "oberth.ci/role=executor-identity"})
	if err != nil {
		t.Fatal(err)
	}
	for _, holder := range holders.Items {
		if len(holder.OwnerReferences) == 1 && holder.OwnerReferences[0].Name == name && holder.DeletionTimestamp == nil {
			t.Fatalf("settled Workflow kept executor identity holder %s", holder.Name)
		}
	}
	argo, err := wfclientset.NewForConfig(cluster.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	results := argo.ArgoprojV1alpha1().WorkflowTaskResults(cluster.namespace)
	remaining := -1
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		list, err := results.List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + name})
		if err != nil {
			t.Fatal(err)
		}
		if remaining = len(list.Items); remaining == 0 {
			break
		}
	}
	excerpt := liveControllerLog(t, cluster, since, name)
	if err := os.WriteFile(filepath.Join(t.ArtifactDir(), "controller-log-excerpt.txt"), excerpt, 0o600); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(excerpt, []byte("failed to delete task-results")) {
		t.Fatalf("Argo's task result cleanup was refused:\n%s", excerpt)
	}
	if remaining != 0 {
		t.Fatalf("%d task results of the fulfilled Workflow remain", remaining)
	}
}

// liveControllerLog returns the workflow-controller log lines since the start
// of the test that name the Workflow.
func liveControllerLog(t *testing.T, cluster liveCluster, since metav1.Time, workflow string) []byte {
	t.Helper()
	pods, err := cluster.kube.CoreV1().Pods(cluster.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: "app=workflow-controller"})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("workflow controller Pod: %d %v", len(pods.Items), err)
	}
	var excerpt bytes.Buffer
	for _, pod := range pods.Items {
		body, err := cluster.kube.CoreV1().Pods(cluster.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{SinceTime: &since}).DoRaw(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, workflow) {
				excerpt.WriteString(line + "\n")
			}
		}
	}
	if excerpt.Len() == 0 {
		t.Fatal("controller log has no line for the Workflow")
	}
	return excerpt.Bytes()
}
