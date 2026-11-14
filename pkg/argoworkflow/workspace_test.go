package argoworkflow

import (
	"strings"
	"testing"
)

func TestAdmitWorkspaceMountsValues(t *testing.T) {
	for _, value := range []string{"none", "cache", "readonly"} {
		t.Run("admits-"+value, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			wf.Spec.Templates[0].Metadata.Annotations = map[string]string{WorkspaceMountsAnnotation: value}
			if err := Admit(wf, Policy{}); err != nil {
				t.Fatalf("%s refused: %v", value, err)
			}
		})
	}
	for _, value := range []string{"", "Cache", "cache ", "none,cache", "caches", "{{inputs.parameters.mode}}"} {
		t.Run("refuses-"+value, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			wf.Spec.Templates[0].Metadata.Annotations = map[string]string{WorkspaceMountsAnnotation: value}
			err := Admit(wf, Policy{})
			if err == nil || !strings.Contains(err.Error(), "must be none, cache or readonly") {
				t.Fatalf("%q: err=%v", value, err)
			}
		})
	}
}
