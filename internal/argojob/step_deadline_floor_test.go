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
		if got := DefaultStepStartupAllowance.Floored(declared); got != want {
			t.Fatalf("DefaultStepStartupAllowance.Floored(%d) = %d, want %d", declared, got, want)
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
	if fmtTemplate.ActiveDeadlineSeconds == nil || fmtTemplate.ActiveDeadlineSeconds.String() != "300" || fmtTemplate.Metadata.Annotations[StepDeadlineAnnotation] != "180" || fmtTemplate.Metadata.Annotations[StepStartAllowanceAnnotation] != "120" {
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
	for _, annotation := range []string{StepDeadlineAnnotation, StepStartAllowanceAnnotation} {
		source := strings.Replace(stepDeadlineDocument, "    - name: open\n      container:", "    - name: open\n      metadata:\n        annotations:\n          "+annotation+": \"999\"\n      container:", 1)
		if _, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source)); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") {
			t.Fatalf("repository-authored %s admitted: %v", annotation, err)
		}
	}
}

// The administrator owns the allowance: factor, floor and ceiling are
// configurable, zero disables it, and the defaults are unchanged.
func TestStepStartupAllowanceIsConfigurable(t *testing.T) {
	custom := StepStartupAllowance{FactorPercent: 100, MinSeconds: 30, MaxSeconds: 900}
	for declared, want := range map[int64]int64{10: 40, 60: 120, 600: 1200, 1200: 2100, 1 << 62: 1<<62 + 900} {
		if got := custom.Floored(declared); got != want {
			t.Fatalf("custom Floored(%d) = %d, want %d", declared, got, want)
		}
	}
	for _, off := range []StepStartupAllowance{{}, {FactorPercent: 50, MinSeconds: 0, MaxSeconds: 0}} {
		if got := off.Floored(180); got != 180 {
			t.Fatalf("disabled allowance %+v widened 180 to %d", off, got)
		}
	}
	if got := (StepStartupAllowance{MinSeconds: 90, MaxSeconds: 90}).Floored(3600); got != 3690 {
		t.Fatalf("fixed allowance = %d", got)
	}
	if (1<<63 - 1) != (StepStartupAllowance{FactorPercent: 400, MaxSeconds: 3600}).Floored(1<<63-1) {
		t.Fatal("unbounded declared value overflowed")
	}
	for _, invalid := range []StepStartupAllowance{
		{FactorPercent: -1, MaxSeconds: 600}, {FactorPercent: 401, MaxSeconds: 600},
		{MinSeconds: -1, MaxSeconds: 600}, {MaxSeconds: 3601}, {MinSeconds: 601, MaxSeconds: 600},
	} {
		config := testConfig()
		config.StepStartupAllowance = &invalid
		if config.Validate() == nil {
			t.Fatalf("invalid allowance accepted: %+v", invalid)
		}
	}

	config := testConfig()
	config.StepStartupAllowance = &custom
	wf, err := Build(config, testRequest(pipeline.TriggerCI, stepDeadlineDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		if template.Name == "fmt" && (template.ActiveDeadlineSeconds.String() != "360" || template.Metadata.Annotations[StepStartAllowanceAnnotation] != "180") {
			t.Fatalf("custom allowance not applied: %v %v", template.ActiveDeadlineSeconds, template.Metadata.Annotations)
		}
	}

	config.StepStartupAllowance = &StepStartupAllowance{}
	wf, err = Build(config, testRequest(pipeline.TriggerCI, stepDeadlineDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		if template.Name == "fmt" && (template.ActiveDeadlineSeconds.String() != "180" || template.Metadata.Annotations[StepDeadlineAnnotation] != "" || template.Metadata.Annotations[StepStartAllowanceAnnotation] != "") {
			t.Fatalf("disabled allowance changed the step: %v %v", template.ActiveDeadlineSeconds, template.Metadata.Annotations)
		}
	}
}

// Classification reads the allowance Build recorded, not the server's current
// setting, so a values change never reclassifies Pods already running.
func TestStartupDeadlineUsesTheRecordedAllowance(t *testing.T) {
	wf, node, pod := startupDeadlineFixture(180, 39*time.Second, 137)
	pod.Annotations[StepStartAllowanceAnnotation] = "30"
	pod.Spec.ActiveDeadlineSeconds = ptr.To(int64(210))
	finished := pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt
	pod.Status.StartTime = ptr.To(metav1.NewTime(finished.Add(-210 * time.Second)))
	if reason := podStartupDeadlineReason(wf, node, pod, "test", "test"); !model.IsStartupDeadlineReason(reason) {
		t.Fatal("a Pod floored by a non-default allowance was not classified")
	}
}

// A document without its own workflow deadline gets the administrator's
// default, still capped by the workflow ceiling.
func TestDefaultWorkflowTimeoutIsConfigurable(t *testing.T) {
	config := testConfig()
	config.DefaultWorkflowTimeout = 45 * time.Minute
	if got := config.ExecutionTimeout("", nil); got != 45*time.Minute {
		t.Fatalf("default workflow timeout = %s", got)
	}
	config.WorkflowTimeout = 20 * time.Minute
	if got := config.ExecutionTimeout("", nil); got != 20*time.Minute {
		t.Fatalf("default not capped by the ceiling: %s", got)
	}
	config.DefaultWorkflowTimeout = 1500 * time.Millisecond
	if config.Validate() == nil {
		t.Fatal("fractional default workflow timeout accepted")
	}
	if got := testConfig().ExecutionTimeout("", nil); got != DefaultExecutionTimeout {
		t.Fatalf("unset default = %s", got)
	}
}

// startupDeadlineFixture is a step Pod killed by its own floored deadline.
func startupDeadlineFixture(declared int64, runtime time.Duration, exit int32) (*wfv1.Workflow, wfv1.NodeStatus, *corev1.Pod) {
	wf, node, pod, _ := diagnosticFixture()
	pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, "DeadlineExceeded"
	pod.Status.Message = "Pod was active on the node longer than the specified deadline"
	pod.Spec.ActiveDeadlineSeconds = ptr.To(DefaultStepStartupAllowance.Floored(declared))
	pod.Annotations[StepDeadlineAnnotation] = strconv.FormatInt(declared, 10)
	pod.Annotations[StepStartAllowanceAnnotation] = strconv.FormatInt(DefaultStepStartupAllowance.Seconds(declared), 10)
	terminated := pod.Status.ContainerStatuses[0].State.Terminated
	terminated.Reason, terminated.ExitCode = "Error", exit
	if exit == 0 {
		terminated.Reason = "Completed"
	}
	terminated.FinishedAt = metav1.NewTime(terminated.StartedAt.Add(runtime))
	// The Pod deadline stopped main: the Pod started one floored deadline
	// before main finished.
	pod.Status.StartTime = ptr.To(metav1.NewTime(terminated.FinishedAt.Add(-time.Duration(DefaultStepStartupAllowance.Floored(declared)) * time.Second)))
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
		"no recorded allowance": func(_ *wfv1.Workflow, p *corev1.Pod) {
			delete(p.Annotations, StepStartAllowanceAnnotation)
		},
		"allowance does not match the Pod deadline": func(_ *wfv1.Workflow, p *corev1.Pod) {
			p.Annotations[StepStartAllowanceAnnotation] = "119"
		},
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

// Kubelet's TerminatePod (pkg/kubelet/status/status_manager.go) rewrites every
// non-terminated container of an initialized Pod to ContainerStatusUnknown
// with exit 137 and no start time. That is how a never-started main looks on a
// real cluster after a Pod deadline kill.
func TestStartupDeadlineNeverStartedShapes(t *testing.T) {
	unknown := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", Message: "The container could not be located when the pod was terminated", ExitCode: 137}}
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	}
	for _, tc := range []struct {
		name  string
		main  corev1.ContainerStatus
		infra bool
	}{
		{"terminate pod rewrite", corev1.ContainerStatus{Name: "main", State: unknown}, true},
		{"container creating", corev1.ContainerStatus{Name: "main", State: waiting("ContainerCreating")}, true},
		{"pod initializing", corev1.ContainerStatus{Name: "main", State: waiting("PodInitializing")}, true},
		{"image pull error", corev1.ContainerStatus{Name: "main", State: waiting("ErrImagePull")}, false},
		{"image pull back-off", corev1.ContainerStatus{Name: "main", State: waiting("ImagePullBackOff")}, false},
		{"invalid image name", corev1.ContainerStatus{Name: "main", State: waiting("InvalidImageName")}, false},
		{"container config error", corev1.ContainerStatus{Name: "main", State: waiting("CreateContainerConfigError")}, false},
		{"unknown waiting reason", corev1.ContainerStatus{Name: "main", State: waiting("")}, false},
		{"unknown after it ran", corev1.ContainerStatus{Name: "main", ContainerID: "containerd://abc", State: unknown}, false},
		{"unknown with a start time", corev1.ContainerStatus{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", ExitCode: 137, StartedAt: metav1.Now()}}}, false},
		{"unknown with another exit", corev1.ContainerStatus{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", ExitCode: 1}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf, node, pod := startupDeadlineFixture(180, 0, 137)
			pod.Status.ContainerStatuses[0] = tc.main
			reason := podStartupDeadlineReason(wf, node, pod, "test", "test")
			if model.IsStartupDeadlineReason(reason) != tc.infra {
				t.Fatalf("startup deadline = %q, want infrastructure %v", reason, tc.infra)
			}
		})
	}
	// The realistic shape goes through the whole completion path too.
	wf, _, pod := startupDeadlineFixture(180, 0, 137)
	pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "main", State: unknown}
	controller := &Controller{kube: fake.NewClientset(pod)}
	completion := completionFor(wf)
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	if !completion.Infrastructure || !model.IsStartupDeadlineReason(completion.Reason) {
		t.Fatalf("never-started main after TerminatePod is not infrastructure: %#v", completion)
	}
}

// A step whose command failed on its own before the Pod deadline is a genuine
// failure even when the Pod later hit its deadline (for example while the wait
// sidecar was still uploading logs).
func TestStartupDeadlineRequiresTheDeadlineToStopMain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   int32
		reason string
		early  time.Duration // how long before the Pod deadline main finished
		start  bool          // Pod start time recorded
		infra  bool
	}{
		{"exit 1 long before the deadline", 1, "Error", 60 * time.Second, true, false},
		{"exit 137 long before the deadline", 137, "Error", 60 * time.Second, true, false},
		{"killed at the deadline", 137, "Error", 0, true, true},
		{"terminated at the deadline", 143, "Error", 0, true, true},
		{"finished within clock skew", 137, "Error", time.Second, true, true},
		{"out of memory at the deadline", 137, "OOMKilled", 0, true, false},
		{"no start time, killed", 137, "Error", 60 * time.Second, false, true},
		{"no start time, exit 1", 1, "Error", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf, node, pod := startupDeadlineFixture(180, 5*time.Second, tc.exit)
			main := pod.Status.ContainerStatuses[0].State.Terminated
			main.Reason = tc.reason
			if tc.start {
				pod.Status.StartTime = ptr.To(metav1.NewTime(pod.Status.StartTime.Add(tc.early)))
			} else {
				pod.Status.StartTime = nil
			}
			reason := podStartupDeadlineReason(wf, node, pod, "test", "test")
			if model.IsStartupDeadlineReason(reason) != tc.infra {
				t.Fatalf("startup deadline = %q, want infrastructure %v", reason, tc.infra)
			}
		})
	}
}
