package argoworkflow

import (
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// GuestTestTemplatesAnnotation requests admin-granted VM test leaves. It grants
// no host capability: the named leaves must also satisfy the KVM contract.
const GuestTestTemplatesAnnotation = "oberth.ci/guest-test-templates"

func DeclaredGuestTestTemplates(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, fmt.Errorf("argoworkflow: no Workflow to read")
	}
	value, declared := workflow.Annotations[GuestTestTemplatesAnnotation]
	if !declared {
		return nil, nil
	}
	kvm, err := DeclaredKVMTemplates(workflow)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(kvm))
	for _, name := range kvm {
		allowed[name] = true
	}
	names := strings.Split(value, ",")
	if len(names) > MaxTemplates {
		return nil, fmt.Errorf("argoworkflow: %s exceeds %d templates", GuestTestTemplatesAnnotation, MaxTemplates)
	}
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		name = strings.TrimSpace(name)
		if !allowed[name] || seen[name] {
			return nil, fmt.Errorf("argoworkflow: %s must name unique KVM leaves; invalid template %q", GuestTestTemplatesAnnotation, name)
		}
		names[i], seen[name] = name, true
	}
	return names, nil
}
