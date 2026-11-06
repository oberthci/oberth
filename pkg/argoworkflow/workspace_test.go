package argoworkflow

import (
	"strings"
	"testing"
)

func TestAdmitWorkspaceMountsValues(t *testing.T) {
	for _, value := range []string{"none", "cache", "readonly"} {
		t.Run("admits-"+value, func(t *testing.T) {
			wf := nonrootDocument(t)
			delete(wf.Annotations, NonrootTemplatesAnnotation)
			wf.Spec.Templates[0].Metadata.Annotations = map[string]string{WorkspaceMountsAnnotation: value}
			if err := Admit(wf, Policy{}); err != nil {
				t.Fatalf("%s refused: %v", value, err)
			}
		})
	}
	for _, value := range []string{"", "Cache", "cache ", "none,cache", "caches", "{{inputs.parameters.mode}}"} {
		t.Run("refuses-"+value, func(t *testing.T) {
			wf := nonrootDocument(t)
			delete(wf.Annotations, NonrootTemplatesAnnotation)
			wf.Spec.Templates[0].Metadata.Annotations = map[string]string{WorkspaceMountsAnnotation: value}
			err := Admit(wf, Policy{})
			if err == nil || !strings.Contains(err.Error(), "must be none, cache or readonly") {
				t.Fatalf("%q: err=%v", value, err)
			}
		})
	}
}

// Nonroot leaves move their Go caches into a private scratch volume and never
// mount the host cache; a cache request would be silently void, so it is
// refused. none stays admitted.
func TestAdmitNonrootRefusesWorkspaceCache(t *testing.T) {
	wf := nonrootDocument(t)
	wf.Spec.Templates[0].Metadata.Annotations = map[string]string{WorkspaceMountsAnnotation: WorkspaceMountsCache}
	if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "nonroot leaves take no host cache mount") {
		t.Fatalf("err=%v", err)
	}
	wf.Spec.Templates[0].Metadata.Annotations[WorkspaceMountsAnnotation] = WorkspaceMountsNone
	if err := Admit(wf, Policy{}); err != nil {
		t.Fatalf("nonroot none refused: %v", err)
	}
}
