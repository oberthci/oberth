package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"strings"
	"testing"
)

const originalDiagnostic = "oberth-hold: command exited 7; held until about 2026-01-01T00:20:00Z; release with run_hold_abandon"

func TestBOriginalDiagnosticIsNotNativeExit(t *testing.T) {
	t.Parallel()
	for _, code := range []int32{7, 64, 70, 137} {
		p := &core.Pod{Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code, Message: originalDiagnostic}}}}}}
		if code == 137 {
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
		}
		d := diagnose(wf.NodeStatus{Phase: wf.NodeFailed}, p, false, &wf.Template{Container: &core.Container{}})
		if d.exit == nil || *d.exit != int(code) || strings.Count(d.detail, originalDiagnostic) != 1 {
			t.Fatal("original status replaced native result", code, d)
		}
		if code == 137 && d.reason != reasonOOM {
			t.Fatal(d)
		}
	}
	checkNativeDiagnosticPrecedence(t)
}
func checkNativeDiagnosticPrecedence(t *testing.T) {
	t.Helper()
	p := &core.Pod{Status: core.PodStatus{Reason: "Evicted", Message: "native eviction", ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7, Message: originalDiagnostic}}}}}}
	d := diagnose(wf.NodeStatus{Phase: wf.NodeFailed}, p, false, &wf.Template{Container: &core.Container{}})
	if d.reason != reasonEvicted || d.exit != nil || d.detail != "native eviction\n"+originalDiagnostic {
		t.Fatal("native precedence changed", d)
	}
	absent := diagnose(wf.NodeStatus{Phase: wf.NodeError, Message: "pod deleted"}, nil, false, nil)
	if absent.exit != nil || strings.Contains(absent.detail, "command exited") {
		t.Fatal("missing diagnostic invented", absent)
	}
}
func TestBTerminationDiagnosticLoss(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"output copy failed", "restarted; command not rerun", strings.Repeat("x", 768), ""} {
		p := &core.Pod{Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 64, Message: message}}}}}}
		d := diagnose(wf.NodeStatus{Phase: wf.NodeFailed}, p, false, &wf.Template{Container: &core.Container{}})
		if d.exit == nil || *d.exit != 64 || strings.Contains(d.detail, "command exited 7") {
			t.Fatal("lost diagnostic restored as a fact", d)
		}
	}
	p := &core.Pod{Status: core.PodStatus{Reason: "DeadlineExceeded", Message: "native deadline", ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7, Message: originalDiagnostic}}}}}}
	d := diagnose(wf.NodeStatus{Phase: wf.NodeFailed}, p, false, &wf.Template{Container: &core.Container{}})
	if d.reason != reasonDeadline || !strings.HasPrefix(d.detail, "native deadline\n") {
		t.Fatal(d)
	}
}
