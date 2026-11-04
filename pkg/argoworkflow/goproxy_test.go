package argoworkflow

import (
	"strings"
	"testing"
)

func TestGoProxyAnnotationAdmission(t *testing.T) {
	for _, value := range []string{"mirror-only", "", "direct", "default", "mirror_only", " mirror-only", "mirror-only\n"} {
		t.Run(value, func(t *testing.T) {
			workflow, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			workflow.Annotations = map[string]string{GoProxyAnnotation: value}
			err = Admit(workflow, Policy{})
			if value == GoProxyMirrorOnly {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), `oberth.ci/goproxy must be "mirror-only" when present`) {
				t.Fatalf("invalid annotation lacks a refusal reason: %v", err)
			}
		})
	}
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatal(err)
	}
	if mirrorOnly, err := DeclaredGoProxyMirrorOnly(workflow); mirrorOnly || err != nil {
		t.Fatalf("omitted annotation changed default: %v %v", mirrorOnly, err)
	}
}
