package admissioncontract

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSealErrorsAlwaysExplainReasonAndRemedy(t *testing.T) {
	for _, err := range []*SealError{{}, {Path: "workflow"}, {Path: "seal"}, {Path: "view"}, {Path: "child", reason: childBindingReason}, {Path: "child", reason: childDigestReason}, {Path: "child", reason: childSpecReason}} {
		for text, limit := range map[string]int{err.Reason(): 80, err.Remedy(): 120} {
			if problem := publicerr.CheckMessage(text, limit); problem != nil {
				t.Fatal(problem, text)
			}
		}
		if err.Reason() == "" || err.Remedy() == "" || err.Error() == "" {
			t.Fatal("incomplete refusal")
		}
	}
}
