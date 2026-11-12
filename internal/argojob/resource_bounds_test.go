package argojob

import (
	"strings"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func resourceBoundsSource(resources string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: XL
spec:
  entrypoint: step-abc12345
  activeDeadlineSeconds: 3600
  templates:
  - name: step-abc12345
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      command: [/bin/true]
` + resources
}
func resourceBoundsRequest(source string) Request {
	r := testRequest(pipeline.TriggerCI, source)
	r.Repo = "repo-01"
	r.UpstreamName = "repo-01"
	r.UpstreamOrg = "repo-01"
	r.ApprovedSecrets = map[string]bool{}
	return r
}
