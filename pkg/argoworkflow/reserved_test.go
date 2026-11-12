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

func TestAdmitRefusesReservedWorkflowMetadata(t *testing.T) {
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
			expect: `spec.workflowMetadata.annotations["` + ReservedMetadataPrefix + `run-id"] is refused`,
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
			name: "workflowMetadata argo controller instance label",
			spec: `  workflowMetadata:
    labels:
      workflows.argoproj.io/controller-instanceid: elsewhere
`,
			expect: `spec.workflowMetadata.labels["workflows.argoproj.io/controller-instanceid"] is refused: keys under workflows.argoproj.io/ are assigned by the Argo controller`,
		},
		{
			name: "workflowMetadata argo annotation",
			spec: `  workflowMetadata:
    annotations:
      workflows.argoproj.io/pod-name-format: v1
`,
			expect: `spec.workflowMetadata.annotations["workflows.argoproj.io/pod-name-format"] is refused`,
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

// A semaphore limit is read only from oberth-sync-limits in the Workflow's
// namespace: the controller echoes an unparsable value into the status, so
// another ConfigMap (or namespace, or a database) would leak through a run.
func TestAdmitRefusesSemaphoresOutsideTheSyncLimits(t *testing.T) {
	for name, semaphore := range map[string]string{
		"other ConfigMap": "      - configMapKeyRef:\n          name: app-settings\n          key: password\n",
		"other namespace": "      - configMapKeyRef:\n          name: oberth-sync-limits\n          key: kvm\n        namespace: kube-system\n",
		"database":        "      - database:\n          key: kvm\n",
	} {
		t.Run(name, func(t *testing.T) {
			document := reservedBase + "  synchronization:\n    semaphores:\n" + semaphore + "  templates:" + reservedLeaf
			refuse(t, document, Policy{}, "semaphores[0] is refused")
			template := reservedBase + "  templates:" + reservedLeaf + "      synchronization:\n        semaphores:\n" + indent(semaphore, "    ")
			refuse(t, template, Policy{}, "semaphores[0] is refused")
		})
	}
}

func indent(text, prefix string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "")
}
