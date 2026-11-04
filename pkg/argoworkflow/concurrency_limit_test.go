package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestDeclaredConcurrencyLimit(t *testing.T) {
	for _, tc := range []struct {
		group, value string
		want         int
		bad          bool
	}{
		{"", "", 1, false}, {"state", "", 1, false}, {"state", "2", 2, false},
		{"state", " 12 ", 12, false}, {"state", "0", 0, true}, {"state", "-1", 0, true},
		{"state", "1.5", 0, true}, {"state", "huge", 0, true}, {"state", "2147483648", 0, true},
		{"", "2", 0, true}, {"state", "+2", 0, true},
	} {
		t.Run(tc.group+"/"+tc.value, func(t *testing.T) {
			w := &wfv1.Workflow{}
			w.Annotations = map[string]string{ConcurrencyGroupAnnotation: tc.group}
			if tc.value != "" {
				w.Annotations[ConcurrencyLimitAnnotation] = tc.value
			}
			got, err := DeclaredConcurrencyLimit(w)
			if (err != nil) != tc.bad || !tc.bad && got != tc.want {
				t.Fatalf("limit=%d err=%v", got, err)
			}
		})
	}
}
