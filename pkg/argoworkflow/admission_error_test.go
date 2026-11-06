package argoworkflow

import (
	"errors"
	"testing"
)

func TestDocumentRefusalsAreNotTransientInfrastructure(t *testing.T) {
	_, decodeErr := Decode([]byte("not a workflow"))
	for _, err := range []error{decodeErr, Admit(nil, Policy{})} {
		var classification interface{ InfrastructureFailure() bool }
		if err == nil || !errors.As(err, &classification) || classification.InfrastructureFailure() {
			t.Fatalf("unclassified document refusal: %v", err)
		}
	}
}
