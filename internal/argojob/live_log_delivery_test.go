package argojob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runlog"
)

// Opt-in, disposable-cluster check. Compare every attempt, including an actual
// retry, concurrent noisy output, silence, and an unterminated command tail.
func TestLiveLogDeliveryMatchesPodOutput(t *testing.T) {
	cluster := requireLiveCluster(t)
	config := cluster.config()
	config.applyDefaults()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runID := fmt.Sprintf("oberth-live-logs-%d", time.Now().UnixNano())
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: runID, Namespace: cluster.namespace, Annotations: map[string]string{runIDAnnotation: runID, identityAnnotation: "live-log-delivery", PodNameVersionAnnotation: podNameVersionV2}}, Spec: wfv1.WorkflowSpec{
		Entrypoint: "logs", ServiceAccountName: config.PipelineServiceAccount, Executor: &wfv1.ExecutorConfig{ServiceAccountName: config.ExecutorServiceAccount}, AutomountServiceAccountToken: ptr.To(false), ActiveDeadlineSeconds: ptr.To(int64(120)),
		Templates: []wfv1.Template{
			{Name: "logs", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "noisy", Template: "noisy"}, {Name: "quiet", Template: "quiet"}, {Name: "retry", Template: "retry"}}}},
			{Name: "noisy", Container: &corev1.Container{Image: "busybox:1.37", Command: []string{"sh", "-c"}, Args: []string{`i=0; while [ "$i" -lt 6 ]; do printf 'line-%03d\n' "$i"; i=$((i+1)); sleep 1; done`}}},
			{Name: "quiet", Container: &corev1.Container{Image: "busybox:1.37", Command: []string{"sh", "-c"}, Args: []string{`sleep 4; printf 'quiet-tail'`}}},
			{Name: "retry", RetryStrategy: &wfv1.RetryStrategy{Limit: ptr.To(intstr.FromInt32(1))}, Container: &corev1.Container{Image: "busybox:1.37", Command: []string{"sh", "-c"}, Args: []string{`printf 'attempt-%s\n' "$ATTEMPT"; test "$ATTEMPT" != 0`}, Env: []corev1.EnvVar{{Name: "ATTEMPT", Value: "{{retries}}"}}}},
		},
	}}
	if err := projectExecutorTokens(workflow, config); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.workflows.Create(ctx, workflow, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		policy := metav1.DeletePropagationBackground
		_ = cluster.workflows.Delete(cleanupCtx, runID, metav1.DeleteOptions{PropagationPolicy: &policy})
	})
	controller, err := NewController(cluster.workflows, cluster.kube, config)
	if err != nil {
		t.Fatal(err)
	}
	controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), config))
	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create(runID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result := make(chan error, 1)
	go func() {
		completion, err := controller.Wait(ctx, runID, runID, file)
		if err == nil && !completion.Succeeded {
			err = fmt.Errorf("workflow failed: %s", completion.Reason)
		}
		result <- err
	}()
	sawLive := false
	var observedAt time.Time
	for !sawLive {
		select {
		case err := <-result:
			t.Fatalf("finished before live output observation: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
		chunk, _, _, err := store.ReadFrom(runID, 0, 1<<20)
		if err != nil {
			continue
		}
		if bytes.Contains(chunk, []byte("line-000")) {
			wf, err := cluster.workflows.Get(ctx, runID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if wf.Status.Phase.Completed() {
				t.Fatal("first output delivered only after completion")
			}
			sawLive = true
			observedAt = time.Now()
		}
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	final, err := cluster.workflows.Get(ctx, runID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex(runID); err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{}
	attempts := stepAttempts(final)
	if len(attempts) != 4 {
		t.Fatalf("expected noisy, quiet, and two retry attempts; got %d", len(attempts))
	}
	for _, step := range attempts {
		stream, err := cluster.kube.CoreV1().Pods(cluster.namespace).GetLogs(step.PodName, &corev1.PodLogOptions{Container: mainContainerName, Timestamps: true}).Stream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
			stamp, text, ok := strings.Cut(line, " ")
			if !ok {
				t.Fatalf("no timestamp: %q", line)
			}
			if text == "line-000" {
				emittedAt, err := time.Parse(time.RFC3339Nano, stamp)
				if err != nil {
					t.Fatal(err)
				}
				lag := observedAt.Sub(emittedAt)
				t.Logf("first numbered line retained after %s", lag)
				if lag > 5*time.Second {
					t.Errorf("healthy local delivery exceeded 5s: %s", lag)
				}
			}
			key := step.Burn + "/" + step.Step
			expected[key] = append(expected[key], "["+key+"] "+text)
		}
	}
	for key, want := range expected {
		burn, step, _ := strings.Cut(key, "/")
		body, err := store.Read(runID, burn, step)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
			if strings.Contains(line, "] oberth: Argo retry attempt ") {
				continue
			}
			got = append(got, line)
		}
		// Retry attempts of the same logical step drain in attempt order.
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s retained output differs from timestamped pod logs\ngot: %q\nwant:%q", key, got, want)
		}
		t.Logf("%s: %d retained lines match pod logs", key, len(got))
	}
}
