package argoworkflow

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"k8s.io/utils/ptr"
)

func (g *graph) runPriority(class string, priority *int32, path string, override bool) {
	if class == "" {
		if ptr.Deref(priority, 0) != 0 && !override {
			g.priorityAdvisory(path, "workflow priority has no Pod class", "Choose a matching native podPriorityClassName; Argo and Kubernetes order separate queues.")
		}
		return
	}
	for _, tier := range []struct {
		name     string
		priority int32
	}{{"release", 30}, {"promotion", 20}, {"qualification", 10}, {"lint", 0}} {
		if g.policy.ServerNamespace == "" || class != "oberth-run-"+g.policy.ServerNamespace+"-"+tier.name {
			continue
		}
		value := ptr.Deref(priority, 0)
		if !override && value != tier.priority {
			g.priorityAdvisory(path, "Workflow and Pod priorities differ", "Use Workflow priority 30/20/10/0 for release/promotion/qualification/lint, or choose an explicit custom class.")
		}
		return
	}
	// The existing native quota owns control-class exclusion.
	if g.policy.ServerNamespace != "" && class == "oberth-control-"+g.policy.ServerNamespace {
		return
	}
	g.priorityAdvisory(path, "run priority class is unmanaged", "Verify this class exists and its preemption policy; custom native class choices remain accepted.")
}

func (g *graph) priorityAdvisory(path, reason, remedy string) {
	g.findings = append(g.findings, admissioncontract.Finding{
		Advisory: true, Path: path, Code: "native_priority", Reason: reason, Remedy: remedy,
	})
}
