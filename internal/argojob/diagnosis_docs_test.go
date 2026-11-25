package argojob_test

import (
	"github.com/oberthci/oberth/internal/stepmodel"
	"os"
	"strings"
	"testing"
)

func TestNativeDiagnosisPlanMatchesSourceClassAndFallback(t *testing.T) {
	doc, err := os.ReadFile("../../docs/plans/c5-native-execution.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	source := "| failed source init / authored init without a server receipt | `source delivery failed` (or `failed before starting`) / `init container failed` | " + string(stepmodel.FailureAuthor) + " / " + string(stepmodel.FailureAuthor) + " |"
	for _, want := range []string{source, "| terminal generic start; authored containers never ran | `failed before starting` | infrastructure |", "Image, deadline, node and authored-init diagnoses retain their typed\nreason, failure class and retry hint."} {
		if !strings.Contains(text, want) {
			t.Fatal("native diagnosis plan differs", want)
		}
	}
}
