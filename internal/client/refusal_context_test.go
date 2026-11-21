package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRefusalPreservesRequiredContext(t *testing.T) {
	err := resultError([]byte(`{"code":"forbidden","reason":"Host is not trusted.","remedy":"Verify its fingerprint.","context":["Offered fingerprint: SHA256:example"],"detail":"Optional diagnostics."}`))
	problem := publicerr.Classify(err)
	encoded, marshalErr := json.Marshal(problem)
	if marshalErr != nil || !strings.Contains(string(encoded), `"context":["Offered fingerprint: SHA256:example"]`) || problem.Detail != "Optional diagnostics." {
		t.Fatal("public refusal context lost", string(encoded), marshalErr)
	}
}
