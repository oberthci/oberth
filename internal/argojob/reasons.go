package argojob

import (
	"cmp"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"strconv"
	"strings"
)

const (
	reasonNotReached  = "not reached"
	reasonFailed      = "step failed"
	reasonPodRefused  = "Pod refused by the cluster"
	reasonRunFailed   = "run failed"
	reasonDeleted     = "workflow was deleted"
	reasonObserve     = "native observation failed"
	reasonWaiting     = "waiting to start"
	reasonNode        = "waiting for a node"
	reasonLock        = "waiting for a lock"
	reasonImage       = "image pull failed"
	reasonStarting    = "starting containers"
	reasonBeforeStart = "failed before starting"
	reasonHeld        = "held for debugging"
	reasonOOM         = "out of memory"
	reasonDeadline    = "step timed out"
	reasonSource      = "source delivery failed"
	reasonKeep        = stateport.ReasonRetentionRefused
	reasonInit        = "init container failed"
	reasonShutdown    = "node shut down"
	reasonDisrupted   = "Pod disrupted"
	reasonEvicted     = "Pod evicted"
	reasonStopped     = "stopped"
	reasonSkipped     = "skipped by condition"
	reasonOmitted     = "omitted"
	reasonUnknown     = "unknown"
	reasonPassed      = "passed"
	reasonRunning     = "running"
	reasonExit        = "exited with code %d"

	// detailLockStatus closes the detail of a step waiting for a lock.
	detailLockStatus = "Run oberth lock status to see who holds it."
)

type diagnosis struct {
	reason, detail, container string
	class                     stepmodel.FailureClass
	exit                      *int
}

func diagnose(n wf.NodeStatus, p *core.Pod, stop bool, template *wf.Template) (d diagnosis) {
	defer func() { d.detail = holdDiagnosticDetail(d.detail, p) }()
	n.Message = currentMessage(n)
	d = diagnosis{reason: reasonUnknown, detail: n.Message, container: "main", class: stepmodel.FailureUnknown}
	if p != nil && diagnosePod(&d, p, template) {
		d = nativeDiagnosis(d, n)
		if !stop {
			d = beforeStartDiagnosis(d, n, p, template)
		}
		return d
	}
	if nativeExit(&d, n) {
		return nativeDiagnosis(d, n)
	}
	if stop && terminalNode(n) {
		d.reason, d.class = reasonStopped, stepmodel.FailureCancelled
		return d
	}
	if p == nil && n.Type == wf.NodeTypePod && terminalNode(n) && strings.Contains(n.Message, "is forbidden") {
		d.reason, d.class = reasonPodRefused, stepmodel.FailureInfrastructure
		if strings.Contains(n.Message, "violates PodSecurity") {
			d.class = stepmodel.FailureAuthor
		}
		return d
	}
	d.reason = nodeReason(n.Phase)
	return d
}

// currentMessage drops the message Argo keeps from before the start, such as
// PodInitializing, on a node that later succeeded; it no longer describes the step.
func currentMessage(n wf.NodeStatus) string {
	if n.Phase == wf.NodeSucceeded {
		return ""
	}
	return n.Message
}
func diagnosePod(d *diagnosis, p *core.Pod, template *wf.Template) bool {
	d.detail = p.Status.Message
	if typedPodReason(d, p) {
		return true
	}
	for _, c := range p.Status.InitContainerStatuses {
		if c.State.Terminated != nil && c.State.Terminated.ExitCode != 0 {
			d.container, d.detail, d.reason, d.class = c.Name, c.State.Terminated.Message, reasonInit, stepmodel.FailureAuthor
			d.exit = new(int(c.State.Terminated.ExitCode))
			if c.Name == "init-artifact-oberth" {
				d.reason, d.class = reasonSource, stepmodel.FailureInfrastructure
			}
			return true
		}
	}
	for _, c := range stepContainerStatuses(template, p) {
		if diagnoseContainer(d, c) {
			if c.Name == "wait" {
				d.class = stepmodel.FailureUnknown
			}
			return true
		}
	}
	return false
}

func beforeStartDiagnosis(d diagnosis, n wf.NodeStatus, p *core.Pod, template *wf.Template) diagnosis {
	switch d.reason {
	case reasonStarting, reasonUnknown, reasonFailed, reasonSource:
		if terminalNode(n) && ownContainersWaiting(template, p) {
			d.reason = reasonBeforeStart
			if d.class == stepmodel.FailureUnknown {
				d.class = stepmodel.FailureInfrastructure
			}
		}
	}
	return d
}

func terminalNode(n wf.NodeStatus) bool {
	return n.Phase == wf.NodeFailed || n.Phase == wf.NodeError
}

func ownContainersWaiting(template *wf.Template, p *core.Pod) bool {
	statuses := authoredContainerStatuses(template, p)
	if len(statuses) == 0 {
		return false
	}
	for _, c := range statuses {
		if c.State.Waiting == nil || c.LastTerminationState.Terminated != nil || c.RestartCount != 0 {
			return false
		}
	}
	return true
}

func typedPodReason(d *diagnosis, p *core.Pod) bool {
	switch p.Status.Reason {
	case "DeadlineExceeded":
		d.reason, d.class = reasonDeadline, stepmodel.FailureDeadline
		return true
	case "NodeShutdown":
		d.reason, d.class = reasonShutdown, stepmodel.FailureInfrastructure
		return true
	case "Evicted":
		d.reason = reasonEvicted
		return true
	}
	for _, c := range p.Status.Conditions {
		if c.Type == core.DisruptionTarget && c.Status == core.ConditionTrue {
			d.reason, d.detail, d.class = reasonDisrupted, c.Message, stepmodel.FailureInfrastructure
			return true
		}
		if c.Type == core.PodScheduled && c.Status == core.ConditionFalse && c.Reason == "Unschedulable" {
			d.reason, d.detail = reasonNode, c.Message
			return true
		}
	}
	return false
}
func diagnoseContainer(d *diagnosis, c core.ContainerStatus) bool {
	d.container = c.Name
	if x := c.State.Terminated; x != nil {
		d.exit = new(int(x.ExitCode))
		d.detail = x.Message
		d.reason, d.class = fmt.Sprintf(reasonExit, x.ExitCode), stepmodel.FailureAuthor
		if x.ExitCode == 0 {
			d.reason = reasonPassed
		}
		if x.Reason == "OOMKilled" {
			d.reason = reasonOOM
		}
		return true
	}
	if x := c.State.Waiting; x != nil {
		d.detail = x.Message
		switch x.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			d.reason = reasonImage
		case "ContainerCreating", "PodInitializing":
			d.reason = reasonStarting
		default:
			d.reason = reasonUnknown
		}
		return true
	}
	return false
}

func nativeExit(d *diagnosis, n wf.NodeStatus) bool {
	if n.Outputs == nil || n.Outputs.ExitCode == nil {
		return false
	}
	value, err := strconv.ParseInt(*n.Outputs.ExitCode, 10, 32)
	if err != nil {
		return false
	}
	d.exit = new(int(value))
	d.reason, d.class = fmt.Sprintf(reasonExit, value), stepmodel.FailureAuthor
	if value == 0 {
		d.reason = reasonPassed
	}
	return true
}

func nodeReason(phase wf.NodePhase) string {
	switch phase {
	case wf.NodePending, "":
		return reasonWaiting
	case wf.NodeRunning:
		return reasonRunning
	case wf.NodeSucceeded:
		return reasonPassed
	case wf.NodeSkipped:
		return reasonSkipped
	case wf.NodeOmitted:
		return reasonOmitted
	case wf.NodeFailed, wf.NodeError:
		return reasonFailed
	}
	return reasonUnknown
}

func authoredContainerStatuses(template *wf.Template, p *core.Pod) []core.ContainerStatus {
	if template == nil {
		return nil
	}
	out := []core.ContainerStatus{}
	for _, c := range p.Status.ContainerStatuses {
		if template.IsMainContainerName(c.Name) {
			out = append(out, c)
		}
	}
	return out
}

func stepContainerStatuses(template *wf.Template, p *core.Pod) []core.ContainerStatus {
	own := authoredContainerStatuses(template, p)
	for _, c := range own {
		if c.State.Terminated != nil && c.State.Terminated.ExitCode != 0 {
			return []core.ContainerStatus{c}
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == "wait" && c.State.Terminated != nil && c.State.Terminated.ExitCode != 0 {
			return []core.ContainerStatus{c}
		}
	}
	return own
}

func nativeDiagnosis(d diagnosis, n wf.NodeStatus) diagnosis {
	d.detail = cmp.Or(d.detail, n.Message)
	if d.exit != nil && d.container != "main" {
		d.detail = strings.TrimSpace(fmt.Sprintf("%s\nExit code: %d", d.detail, *d.exit))
	}
	if (n.Phase == wf.NodeFailed || n.Phase == wf.NodeError) && (d.reason == reasonPassed || d.reason == reasonUnknown) {
		d.reason, d.class = reasonFailed, stepmodel.FailureUnknown
	}
	return d
}

func holdDiagnosticDetail(detail string, p *core.Pod) string {
	if p == nil {
		return detail
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Name != "main" || c.State.Terminated == nil {
			continue
		}
		for _, line := range strings.Split(c.State.Terminated.Message, "\n") {
			if strings.HasPrefix(line, "oberth-hold: command exited ") && !strings.Contains(detail, line) {
				return strings.TrimSpace(detail + "\n" + line)
			}
		}
	}
	return detail
}
