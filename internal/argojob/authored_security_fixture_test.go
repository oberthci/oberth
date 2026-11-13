package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const authoredNonrootDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: test
  activeDeadlineSeconds: 600
  templates:
  - name: test
    securityContext:
      runAsUser: 65534
      runAsGroup: 65534
      runAsNonRoot: true
      seccompProfile: {type: RuntimeDefault}
    container:
      image: golang:1.26.10-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      securityContext:
        runAsUser: 65534
        runAsGroup: 65534
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: [ALL]}
        seccompProfile: {type: RuntimeDefault}
      env:
      - {name: GOCACHE, value: /bad/shared}
      - {name: GOTMPDIR, value: /bad/long/path}
  - name: ordinary
    container:
      image: golang:1.26.10-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`

func authoredNonrootFixture(t *testing.T, script bool) *wfv1.Workflow {
	t.Helper()
	return authoredNonrootWithScratch(t, script, "")
}

func authoredNonrootWithScratch(t *testing.T, script bool, scratch string) *wfv1.Workflow {
	t.Helper()
	document := authoredNonrootDocument
	if script {
		wf, err := argoworkflow.Decode([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &wf.Spec.Templates[0]
		tmpl.Script = &wfv1.ScriptTemplate{Container: *tmpl.Container, Source: "true"}
		tmpl.Script.Command = []string{"/bin/sh"}
		tmpl.Container = nil
		encoded, err := json.Marshal(wf)
		if err != nil {
			t.Fatal(err)
		}
		document = string(encoded)
	}
	config := testConfig()
	config.ExecutorScratchSizeLimit = scratch
	config.CICacheRoot = "/server/cache/ci"
	config.ReleaseCacheRoot = "/server/cache/release"
	request := testRequest(pipeline.TriggerCI, document)
	request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	return wf
}
