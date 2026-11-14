package argojob

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-workflows/v4/util/expr/argoexpr"
	argoexprenv "github.com/argoproj/argo-workflows/v4/util/expr/env"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestNodeRebootClassifiesOwnedUnknownTerminationAcrossResume(t *testing.T) {
	for _, kind := range []string{"other", "unknown", "missing-runtime-state", "empty-255", "old-output-255", "output-255", "unreadable-255", "no-resume-255", "replaced-log-pod", "unchanged-boot", "replacement-node", "node-read-error", "future-termination", "conflicting-exit", "foreign-pod", "no-running-observation", "real-code", "oom", "mixed-code"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "boot.db")
			db, err := storetest.Open(ctx, path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "forge", Kind: "forgejo", BaseURL: "https://forge.example"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := storetest.EnqueueRun(db, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "boot", SHA: strings.Repeat("a", 40), Actor: "test", Trigger: "push"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			wf, node, pod, _ := diagnosticFixtureAt(time.Now().UTC().Add(-time.Second))
			wf.Annotations[runIDAnnotation] = run.ID
			if _, err = db.SetRunWorkflowName(ctx, run.ID, wf.Name); err != nil {
				t.Fatal(err)
			}
			pod.Spec.NodeName = "worker"
			pod.Spec.RestartPolicy = corev1.RestartPolicyNever
			oldPod := pod.DeepCopy()
			oldPod.Status.Phase = corev1.PodRunning
			oldPod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: node.StartedAt}}
			machine := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "33333333-3333-4333-8333-333333333333"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "44444444-4444-4444-8444-444444444444"}}}
			kube := fake.NewClientset(oldPod, machine)
			controller := (&Controller{kube: kube}).WithNodeBootRecorder(db)
			if kind != "no-running-observation" {
				controller.observeRunningNodeBoots(ctx, wf, nil)
			}
			// Reopen canonical storage, as a replacement server process would.
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = storetest.Open(ctx, path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			controller = (&Controller{kube: kube}).WithNodeBootRecorder(db)
			controller.containerLogSource = func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			}
			resumed := time.Now().UTC()
			machine.Status.NodeInfo.BootID = "55555555-5555-4555-8555-555555555555"
			term := pod.Status.ContainerStatuses[0].State.Terminated
			term.Reason = "other"
			term.ExitCode = 255
			term.FinishedAt = metav1.NewTime(time.Now().UTC())
			node.FinishedAt = term.FinishedAt
			node.Outputs.ExitCode = ptr.To("255")
			switch kind {
			case "unknown":
				term.Reason = "ContainerStatusUnknown"
			case "missing-runtime-state":
				term.Reason = "ContainerStatusUnknown"
				term.ContainerID = ""
				term.StartedAt = metav1.Time{}
				pod.Status.ContainerStatuses[0].ContainerID = ""
			case "empty-255", "old-output-255", "output-255", "unreadable-255", "no-resume-255", "replaced-log-pod":
				term.Reason = "Error"
			case "future-termination":
				term.FinishedAt = metav1.NewTime(time.Now().Add(time.Hour))
			case "conflicting-exit":
				term.Reason = "ContainerStatusUnknown"
				term.StartedAt = metav1.Time{}
				node.Outputs.ExitCode = ptr.To("1")
			case "unchanged-boot":
				machine.Status.NodeInfo.BootID = "44444444-4444-4444-8444-444444444444"
			case "replacement-node":
				machine.UID = "66666666-6666-4666-8666-666666666666"
			case "foreign-pod":
				pod.OwnerReferences[0].UID = "66666666-6666-4666-8666-666666666666"
			case "real-code":
				term.Reason = "Error"
				term.ExitCode = 1
				node.Outputs.ExitCode = ptr.To("1")
			case "oom":
				term.Reason = "OOMKilled"
				term.ExitCode = 137
				node.Outputs.ExitCode = ptr.To("137")
			}
			if kind == "output-255" {
				controller.containerLogSource = func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("output")), nil
				}
			}
			if kind == "old-output-255" {
				controller.containerLogSource = func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
					if options.SinceTime == nil || !options.SinceTime.Time.Equal(resumed) || options.Follow || options.Container != "main" {
						t.Fatalf("resume log scope: %+v", options)
					}
					return io.NopCloser(strings.NewReader("")), nil
				}
			}
			if kind == "unreadable-255" {
				controller.containerLogSource = func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
					return nil, context.DeadlineExceeded
				}
			}
			if kind == "no-resume-255" {
				resumed = time.Time{}
			}
			if kind == "replaced-log-pod" {
				controller.containerLogSource = func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
					replacement := pod.DeepCopy()
					replacement.UID = "66666666-6666-4666-8666-666666666666"
					_, err := kube.CoreV1().Pods(pod.Namespace).Update(ctx, replacement, metav1.UpdateOptions{})
					return io.NopCloser(strings.NewReader("")), err
				}
			}
			if kind == "node-read-error" {
				kube.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, machine, context.DeadlineExceeded })
			}
			wf.Status.Nodes[node.ID] = node
			if _, err = kube.CoreV1().Nodes().Update(ctx, machine, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err = kube.CoreV1().Pods(pod.Namespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			step := StepResult{Stage: "test", Step: "unit", NodeID: node.ID, PodName: pod.Name, Status: runprogress.StepFailed, ExitCode: int(term.ExitCode), ExitCodeKnown: true, StartedAt: ptr.To(node.StartedAt.Time), FinishedAt: ptr.To(node.FinishedAt.Time)}
			completion := Completion{Steps: []StepResult{step}, ReplayAttempts: []StepResult{step}, FailedStage: "test", FailedStep: "unit"}
			if kind == "mixed-code" {
				completion.Steps = append(completion.Steps, StepResult{Stage: "test", Step: "real", Status: runprogress.StepFailed})
			}
			controller.collectRestartStepDiagnostics(ctx, wf, &completion, resumed)
			want := kind == "other" || kind == "unknown" || kind == "missing-runtime-state" || kind == "empty-255" || kind == "old-output-255"
			if completion.Infrastructure != want {
				t.Fatalf("infrastructure=%v want %v: diagnostic=%+v direct=%+v", completion.Infrastructure, want, completion.Steps[0].Termination, projectStepDiagnostic(wf, node, pod, time.Now().UTC()))
			}
			if want && (!model.IsNodeRebootReason(completion.Reason) || !strings.Contains(completion.Reason, "node worker restarted after") || !model.IsNodeRebootReason(completion.ReplayAttempts[0].InfrastructureReason)) {
				t.Fatalf("reboot evidence lost: %+v", completion)
			}
		})
	}
}

func TestAssistRefusesUnknownTerminationAnd255(t *testing.T) {
	for _, message := range []string{"main: other (exit code 1)", "main: ContainerStatusUnknown (exit code 255)", "main: Error (exit code 255)", "main: Error (exit code 1)"} {
		code := "1"
		if strings.Contains(message, "255") {
			code = "255"
		}
		scope := map[string]any{"lastRetry.exitCode": code, "lastRetry.duration": "12", "lastRetry.message": message}
		got, err := argoexpr.EvalBool(assistCommandFailure(300), argoexprenv.GetFuncMap(scope))
		if err != nil || got != (message == "main: Error (exit code 1)") {
			t.Fatalf("%s: %v %v", message, got, err)
		}
	}
}
