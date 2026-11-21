package workflowpolicy

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestPolicyErrorsAlwaysExplainReasonAndRemedy(t *testing.T) {
	for _, path := range []string{"", "file", "ref", "repo", "routes", "pattern", "step", "path", "audience"} {
		err := &InvalidError{Path: path}
		for text, limit := range map[string]int{err.Reason(): 80, err.Remedy(): 120} {
			if err := publicerr.CheckMessage(text, limit); err != nil {
				t.Fatal(err, text)
			}
		}
		if err.Reason() == "" || err.Remedy() == "" || err.Error() == "" {
			t.Fatal("incomplete refusal")
		}
	}
}
