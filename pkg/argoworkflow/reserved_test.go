package argoworkflow

import (
	"strings"
	"testing"
)

const reservedBase = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
`

const reservedLeaf = `
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func TestAdmitRefusesReservedWorkflowMetadataAndReleaseMutex(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		template string
		expect   string
	}{
		{
			name: "workflowMetadata annotation run-id",
			spec: `  workflowMetadata:
    annotations:
      oberth.ci/run-id: someone-else
`,
			expect: `spec.workflowMetadata.annotations["oberth.ci/run-id"] is refused`,
		},
		{
			name: "workflowMetadata label",
			spec: `  workflowMetadata:
    labels:
      oberth.ci/run: other
`,
			expect: `spec.workflowMetadata.labels["oberth.ci/run"] is refused`,
		},
		{
			name: "workflowMetadata labelsFrom",
			spec: `  workflowMetadata:
    labelsFrom:
      oberth.ci/trigger:
        expression: "'release'"
`,
			expect: `spec.workflowMetadata.labelsFrom["oberth.ci/trigger"] is refused`,
		},
		{
			name: "workflow-level release mutex",
			spec: `  synchronization:
    mutexes:
      - name: oberth-release
`,
			expect: `spec.synchronization.mutexes[0].name "oberth-release" is reserved`,
		},
		{
			name: "workflow-level release mutex in a named namespace",
			spec: `  synchronization:
    mutexes:
      - name: own
      - name: oberth-release
        namespace: elsewhere
`,
			expect: `spec.synchronization.mutexes[1].name "oberth-release" is reserved`,
		},
		{
			name: "workflowMetadata templated annotation key",
			spec: `  workflowMetadata:
    annotations:
      "{{workflow.parameters.key}}": someone-else
`,
			expect: `spec.workflowMetadata.annotations["{{workflow.parameters.key}}"] is refused`,
		},
		{
			name: "workflowMetadata templated labelsFrom key",
			spec: `  workflowMetadata:
    labelsFrom:
      "{{='oberth.ci/' + 'run'}}":
        expression: "'other'"
`,
			expect: `is refused: a templated key could resolve`,
		},
		{
			name: "workflow-level mutex named by a parameter",
			spec: `  synchronization:
    mutexes:
      - name: "{{workflow.parameters.lock}}"
`,
			expect: `spec.synchronization.mutexes[0].name "{{workflow.parameters.lock}}" is refused`,
		},
		{
			name: "workflow-level mutex completed by an expression",
			spec: `  synchronization:
    mutexes:
      - name: "oberth-{{='release'}}"
`,
			expect: `could resolve to the reserved oberth-release`,
		},
		{
			name:     "template-level mutex named by an input",
			template: "      synchronization:\n        mutexes:\n          - name: \"{{inputs.parameters.lock}}\"\n",
			expect:   `spec.templates["main"].synchronization.mutexes[0].name "{{inputs.parameters.lock}}" is refused`,
		},
		{
			name:     "template-level release mutex",
			template: "      synchronization:\n        mutexes:\n          - name: oberth-release\n",
			expect:   `spec.templates["main"].synchronization.mutexes[0].name "oberth-release" is reserved`,
		},
		{
			name: "templateDefaults release mutex",
			spec: `  templateDefaults:
    synchronization:
      mutexes:
        - name: oberth-release
`,
			expect: `spec.templateDefaults.synchronization.mutexes[0].name "oberth-release" is reserved`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refuse(t, reservedBase+tc.spec+"  templates:"+reservedLeaf+tc.template, Policy{}, tc.expect)
		})
	}
}

func TestAdmitKeepsUserLocksAndUnreservedWorkflowMetadata(t *testing.T) {
	document := reservedBase + `  workflowMetadata:
    labels:
      team: build
    annotations:
      example.org/owner: build
  synchronization:
    mutexes:
      - name: chart-index
      - name: oberth-release-finalize
      - name: oberth-chart-index
      - name: "deploy-{{workflow.parameters.environment}}"
      - name: "oberth-chart-{{workflow.parameters.environment}}"
    semaphores:
      - configMapKeyRef:
          name: oberth-sync-limits
          key: kvm
  templates:` + reservedLeaf + `      synchronization:
        semaphores:
          - configMapKeyRef:
              name: oberth-sync-limits
              key: heavy-go
        mutexes:
          - name: own-lock
`
	mustAdmit(t, document, Policy{})
}

func TestReservedRefusalNamesTheFix(t *testing.T) {
	workflow, err := Decode([]byte(reservedBase + `  workflowMetadata:
    annotations:
      oberth.ci/run-id: x
  templates:` + reservedLeaf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	err = Admit(workflow, Policy{})
	if err == nil || !strings.Contains(err.Error(), "remove the key or rename it outside oberth.ci/") {
		t.Fatalf("refusal must say how to fix it, got %v", err)
	}
}
