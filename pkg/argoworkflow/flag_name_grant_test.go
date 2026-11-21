package argoworkflow

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestAuthoredStepNameThatReadsAsAFlagIsPrintedInItsFlagForm: a pusher names
// a template like a flag; the grant command its finding prints gives that
// name as the step flag's value, never as a positional word oberth would
// read as another selector.
func TestAuthoredStepNameThatReadsAsAFlagIsPrintedInItsFlagForm(t *testing.T) {
	for _, name := range []string{"--step=*", "--secret=prod", "-x"} {
		w := simpleWorkflow()
		old := w.Spec.Templates[0].Name
		w.Spec.Templates[0].Name = name
		if w.Spec.Entrypoint == old {
			w.Spec.Entrypoint = name
		}
		clusterLocation(w, &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "deploy"}, Key: "token"}, "volume")
		_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
		var command string
		if err != nil {
			if _, after, found := strings.Cut(err.Error(), "oberth access allow"); found {
				command, _, _ = strings.Cut(after, "\n")
			}
		}
		if command == "" {
			t.Fatalf("%q: the grant finding is missing: %v", name, err)
		}
		if !strings.Contains(command, " --step='"+name+"' ") || strings.Contains(command, " '"+name+"'") {
			t.Errorf("%q is printed where oberth reads it as a flag: %s", name, command)
		}
	}
}
