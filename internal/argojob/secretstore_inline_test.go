package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestSecretstoreExecCheckedInsideInlineTemplates(t *testing.T) {
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/upstream/acme/oberth/credentials
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      steps:
        - - name: fetch
            inline:
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/run/oberth/bin/oberth]
                args: ["secretstore", "exec", "--path", "oberth/data/release/cosign-secret", "--", "/bin/true"]
`
	request := testRequest(pipeline.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/acme/oberth/credentials": true}
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("an inline secretstore-exec template fetching an undeclared path was admitted")
	}
	if !strings.Contains(err.Error(), "oberth/data/release/cosign-secret") {
		t.Fatalf("refusal does not name the undeclared path: %v", err)
	}
}
