package argojob

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func diagnosticFixture() (*wfv1.Workflow, wfv1.NodeStatus, *corev1.Pod, time.Time) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow-test", Namespace: "pipelines", UID: "11111111-1111-4111-8111-111111111111", Annotations: map[string]string{runIDAnnotation: strings.Repeat("a", 32)}}}
	node := wfv1.NodeStatus{ID: "workflow-test-1234", Name: "workflow-test.test", DisplayName: "test", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, StartedAt: metav1.NewTime(at.Add(-time.Minute)), FinishedAt: metav1.NewTime(at.Add(-time.Second)), Outputs: &wfv1.Outputs{ExitCode: ptr.To("137")}}
	wf.Status.Nodes = wfv1.Nodes{node.ID: node}
	wf.Status.Phase = wfv1.WorkflowFailed
	cid := "containerd://" + strings.Repeat("b", 64)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: wf.Namespace, Name: PodName(wf, node), UID: "22222222-2222-4222-8222-222222222222", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{ActiveDeadlineSeconds: ptr.To(int64(300)), Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}, {Name: "wait"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: cid, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", ContainerID: cid, StartedAt: node.StartedAt, FinishedAt: node.FinishedAt}}}, {Name: "wait", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}}}
	return wf, node, pod, at
}

func TestStepDiagnosticExactCurrentMainAndReason(t *testing.T) {
	wf, node, pod, at := diagnosticFixture()
	d := projectStepDiagnostic(wf, node, pod, at)
	if !d.Available || d.Validate() != nil || d.Container.Reason != "OOMKilled" || d.Container.ExitCode != 137 || *d.Container.MemoryLimitBytes != 1<<30 || *d.Container.CPULimitMilli != 2000 || *d.Container.ActiveDeadlineSeconds != 300 {
		t.Fatalf("projection %#v", d)
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
	d = projectStepDiagnostic(wf, node, pod, at)
	if !d.Available || d.Container.Reason != "Error" {
		t.Fatal("137 inferred OOM")
	}
	pod.Status.Reason = "DeadlineExceeded"
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "secret-canary"
	pod.Status.ContainerStatuses[0].State.Terminated.Message = "observed termination detail"
	d = projectStepDiagnostic(wf, node, pod, at)
	raw, _ := json.Marshal(d)
	if !d.Available || d.Container.Reason != "other" || d.Container.PodReason != "DeadlineExceeded" || d.Container.Message != "observed termination detail" || strings.Contains(string(raw), "secret-canary") {
		t.Fatal("raw reason leaked or Pod deadline conflated")
	}
}

func TestStepDiagnosticRefusesUnboundOrAmbiguousCurrentTermination(t *testing.T) {
	cases := map[string]func(*wfv1.Workflow, *wfv1.NodeStatus, *corev1.Pod){
		"foreign-owner": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" },
		"foreign-run": func(w *wfv1.Workflow, _ *wfv1.NodeStatus, _ *corev1.Pod) {
			w.Annotations[runIDAnnotation] = "secret-canary"
		},
		"foreign-node": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Annotations["workflows.argoproj.io/node-id"] = "foreign"
		},
		"missing-uid": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.UID = "" },
		"duplicate-owner": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.OwnerReferences = append(p.OwnerReferences, p.OwnerReferences[0])
		},
		"sidecar-only": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses = p.Status.ContainerStatuses[1:]
		},
		"last-state-only": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].LastTerminationState = p.Status.ContainerStatuses[0].State
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{}
		},
		"duplicate-main": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		},
		"unsupported-containers": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "authored-other"})
		},
		"changed-cid": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://" + strings.Repeat("c", 64)
		},
		"waiting-and-terminated": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{}
		},
		"future": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(time.Hour))
		},
		"overflow-quantity": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("100000000000000000000000")
		},
		"contradictory-completed": func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"
		},
		"exit-disagreement": func(_ *wfv1.Workflow, n *wfv1.NodeStatus, _ *corev1.Pod) { n.Outputs.ExitCode = ptr.To("0") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			w, n, p, at := diagnosticFixture()
			change(w, &n, p)
			d := projectStepDiagnostic(w, n, p, at)
			raw, _ := json.Marshal(d)
			if d.Available || d.Container != nil || d.Validate() != nil || strings.Contains(string(raw), "secret-canary") {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestStepDiagnosticCollectionPreservesVerdictAndAttempt(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	controller := &Controller{kube: fake.NewClientset(pod)}
	completion := completionFor(wf)
	before := completion.Succeeded
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	if completion.Succeeded != before || len(completion.Steps) != 1 || completion.Steps[0].NodeID != node.ID || !completion.Steps[0].Termination.Available {
		t.Fatal("selected attempt lost")
	}
	completion.Steps[0].AttemptAmbiguous = true
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	if completion.Steps[0].Termination.Unavailable != "ambiguous-attempt" {
		t.Fatal("ambiguity accepted")
	}
	completion.Steps[0].AttemptAmbiguous = false
	controller.kube = fake.NewClientset()
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	if completion.Succeeded != before || completion.Steps[0].Termination.Unavailable != "pod-absent" {
		t.Fatal("read failure changed verdict")
	}
}

func TestStepDiagnosticRetryTieMarkedWithoutChangingSelection(t *testing.T) {
	wf, n, _, _ := diagnosticFixture()
	first := StepResults(wf)[0]
	second := first
	second.NodeID = "other-attempt"
	second.ExitCode = 0
	got := deduplicateRetries([]StepResult{first, second})
	if len(got) != 1 || got[0].NodeID != second.NodeID || got[0].ExitCode != 0 || !got[0].AttemptAmbiguous {
		t.Fatal("tie altered verdict or invented bound attempt")
	}
	later := second.StartedAt.Add(time.Second)
	second.StartedAt = &later
	got = deduplicateRetries([]StepResult{first, second})
	if got[0].AttemptAmbiguous || got[0].NodeID == n.ID {
		t.Fatal("distinct final attempt not retained")
	}
}

func TestStepDiagnosticBlockingTransportUsesOneTotalBudget(t *testing.T) {
	t.Parallel()
	wf, node, _, _ := diagnosticFixture()
	other := node
	other.ID = "workflow-test-5678"
	other.Name = "workflow-test.other"
	other.DisplayName = "other"
	wf.Status.Nodes[other.ID] = other
	var calls atomic.Int32
	joined := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/pipelines/pods" || r.URL.Query().Get("limit") != "128" || r.URL.Query().Get("labelSelector") != "workflows.argoproj.io/workflow="+wf.Name {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		<-r.Context().Done()
		close(joined)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &timingRecorderStub{}
	controller := &Controller{kube: client, timings: recorder}
	completion := completionFor(wf)
	start := time.Now()
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	elapsed := time.Since(start)
	if elapsed < 4900*time.Millisecond || elapsed > 8*time.Second || calls.Load() != 1 || completion.Succeeded {
		t.Fatalf("budget/verdict: elapsed=%s requests=%d succeeded=%v", elapsed, calls.Load(), completion.Succeeded)
	}
	if recorder.writeErr != nil || len(recorder.steps) != 2 {
		t.Fatal("read deadline suppressed durable unknown evidence", recorder.writeErr)
	}
	select {
	case <-joined:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("transport handler did not join")
	}
	if len(completion.Steps) != 2 {
		t.Fatal("lost step")
	}
	for _, step := range completion.Steps {
		if step.Termination.Unavailable != "observation-unavailable" || step.ExitCode != 137 {
			t.Fatal("diagnostic failure masked original failed verdict")
		}
	}
}

func TestStepDiagnosticDuplicateNodeAndAbsentResources(t *testing.T) {
	wf, node, pod, at := diagnosticFixture()
	completion := completionFor(wf)
	wf.Status.Nodes["contradictory-map-key"] = node
	controller := &Controller{kube: fake.NewClientset(pod)}
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	if completion.Steps[0].Termination.Unavailable != "ambiguous-attempt" || completion.Succeeded {
		t.Fatal("duplicate node identity accepted")
	}
	delete(wf.Status.Nodes, "contradictory-map-key")
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	pod.Spec.ActiveDeadlineSeconds = nil
	d := projectStepDiagnostic(wf, node, pod, at)
	if !d.Available || d.Container.CPURequestMilli != nil || d.Container.CPULimitMilli != nil || d.Container.MemoryRequestBytes != nil || d.Container.MemoryLimitBytes != nil || d.Container.ActiveDeadlineSeconds != nil {
		t.Fatal("absent resources invented")
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "memory_limit_bytes") || strings.Contains(string(raw), "active_deadline_seconds") {
		t.Fatal("absent quantity emitted")
	}
	current := &pod.Status.ContainerStatuses[0]
	current.RestartCount = 1
	current.LastTerminationState = *current.State.DeepCopy()
	current.State.Terminated.Reason = "Error"
	d = projectStepDiagnostic(wf, node, pod, at)
	if !d.Available || d.Container.Reason != "Error" || d.Container.RestartCount != 1 {
		t.Fatal("last OOM substituted for current Error")
	}
	current.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}}
	d = projectStepDiagnostic(wf, node, pod, at)
	if d.Available || d.Unavailable != "termination-unavailable" {
		t.Fatal("last termination substituted for restarted running container")
	}
}
