package argojob

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const stepDeadlineDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
    - name: main
      dag:
        tasks:
          - name: fmt
            template: fmt
          - name: inline
            inline:
              activeDeadlineSeconds: 1800
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
          - name: open
            template: open
    - name: fmt
      activeDeadlineSeconds: 180
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
    - name: open
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func TestStepStartAllowanceBounds(t *testing.T) {
	for declared, want := range map[int64]int64{4: 124, 60: 180, 180: 300, 300: 450, 1200: 1800, 3600: 4200} {
		if got := FlooredStepDeadline(declared); got != want {
			t.Fatalf("FlooredStepDeadline(%d) = %d, want %d", declared, got, want)
		}
	}
}

// #1499: Pod deadlines count start-up. Build widens each declared leaf deadline
// by a bounded allowance and records the declared budget on the step Pod.
func TestBuildFloorsStepDeadlinesAndRecordsDeclaredBudget(t *testing.T) {
	wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, stepDeadlineDocument))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*wfv1.Template{}
	for i := range wf.Spec.Templates {
		byName[wf.Spec.Templates[i].Name] = &wf.Spec.Templates[i]
	}
	fmtTemplate := byName["fmt"]
	if fmtTemplate.ActiveDeadlineSeconds == nil || fmtTemplate.ActiveDeadlineSeconds.String() != "300" || fmtTemplate.Metadata.Annotations[StepDeadlineAnnotation] != "180" {
		t.Fatalf("fmt deadline %v annotations %v", fmtTemplate.ActiveDeadlineSeconds, fmtTemplate.Metadata.Annotations)
	}
	inline := byName["main"].DAG.Tasks[1].Inline
	if inline == nil || inline.ActiveDeadlineSeconds.String() != "2400" || inline.Metadata.Annotations[StepDeadlineAnnotation] != "1800" {
		t.Fatalf("inline deadline not floored: %#v", inline)
	}
	if open := byName["open"]; open.ActiveDeadlineSeconds != nil || open.Metadata.Annotations[StepDeadlineAnnotation] != "" {
		t.Fatal("a step without a declared deadline gained one")
	}
	if wf.Spec.ActiveDeadlineSeconds == nil || *wf.Spec.ActiveDeadlineSeconds != int64(DefaultExecutionTimeout/time.Second) {
		t.Fatal("workflow deadline changed")
	}
}

func TestStepDeadlineAnnotationIsServerOwned(t *testing.T) {
	source := strings.Replace(stepDeadlineDocument, "    - name: open\n      container:", "    - name: open\n      metadata:\n        annotations:\n          oberth.ci/step-deadline-seconds: \"999\"\n      container:", 1)
	if _, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source)); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") {
		t.Fatalf("repository-authored step deadline annotation admitted: %v", err)
	}
}

// startupDeadlineFixture is a step Pod killed by its own floored deadline.
func startupDeadlineFixture(declared int64, runtime time.Duration, exit int32) (*wfv1.Workflow, wfv1.NodeStatus, *corev1.Pod) {
	wf, node, pod, _ := diagnosticFixture()
	pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, "DeadlineExceeded"
	pod.Status.Message = "Pod was active on the node longer than the specified deadline"
	pod.Spec.ActiveDeadlineSeconds = ptr.To(FlooredStepDeadline(declared))
	pod.Annotations[StepDeadlineAnnotation] = strconv.FormatInt(declared, 10)
	terminated := pod.Status.ContainerStatuses[0].State.Terminated
	terminated.Reason, terminated.ExitCode = "Error", exit
	if exit == 0 {
		terminated.Reason = "Completed"
	}
	terminated.FinishedAt = metav1.NewTime(terminated.StartedAt.Add(runtime))
	node.Outputs.ExitCode = ptr.To(strconv.Itoa(int(exit)))
	node.Message = pod.Status.Message
	wf.Status.Nodes[node.ID] = node
	return wf, node, pod
}

func TestStartupDeadlineKillIsInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
		runtime  time.Duration
		exit     int32
		infra    bool
	}{
		{"main ran far below its budget", 180, 39 * time.Second, 137, true},
		{"main exited zero", 300, 265 * time.Second, 0, true},
		{"main used its whole budget", 180, 181 * time.Second, 137, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf, _, pod := startupDeadlineFixture(tc.declared, tc.runtime, tc.exit)
			controller := &Controller{kube: fake.NewClientset(pod)}
			completion := completionFor(wf)
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if completion.Infrastructure != tc.infra {
				t.Fatalf("infrastructure = %v, want %v: %#v", completion.Infrastructure, tc.infra, completion)
			}
			if !tc.infra {
				if completion.FailedStep == "" || model.IsStartupDeadlineReason(completion.Reason) {
					t.Fatal("a genuinely slow step lost its own deadline verdict")
				}
				return
			}
			if !model.IsStartupDeadlineReason(completion.Reason) || model.IsExecutionDeadlineReason(completion.Reason) {
				t.Fatalf("reason %q", completion.Reason)
			}
			if len(completion.ReplayAttempts) != 1 || !model.IsStartupDeadlineReason(completion.ReplayAttempts[0].InfrastructureReason) {
				t.Fatal("startup deadline missing from replay evidence")
			}
		})
	}
}

func TestStartupDeadlineRequiresServerBudgetAndStepScope(t *testing.T) {
	cases := map[string]func(*wfv1.Workflow, *corev1.Pod){
		// Argo lowered the Pod deadline to the remaining workflow budget.
		"capped by workflow": func(_ *wfv1.Workflow, p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = ptr.To(int64(60)) },
		"no server budget":   func(_ *wfv1.Workflow, p *corev1.Pod) { delete(p.Annotations, StepDeadlineAnnotation) },
		"workflow deadline": func(w *wfv1.Workflow, _ *corev1.Pod) {
			w.Status.Message = model.ExecutionDeadlineMessage
		},
		"not a deadline kill": func(_ *wfv1.Workflow, p *corev1.Pod) { p.Status.Reason = "" },
		"foreign Pod":         func(_ *wfv1.Workflow, p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			wf, node, pod := startupDeadlineFixture(180, 39*time.Second, 137)
			mutate(wf, pod)
			if reason := podStartupDeadlineReason(wf, node, pod, "test", "test"); reason != "" {
				t.Fatalf("classified as startup deadline: %q", reason)
			}
		})
	}
	wf, node, pod := startupDeadlineFixture(180, 0, 137)
	pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}
	if reason := podStartupDeadlineReason(wf, node, pod, "test", "test"); !model.IsStartupDeadlineReason(reason) {
		t.Fatal("a main container that never started was not infrastructure")
	}
}
