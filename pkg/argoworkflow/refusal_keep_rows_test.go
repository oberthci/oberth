package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

type keepRefusalCase struct{ row, section, insert, reason string }

// One independent hostile fixture for every KEEP row in sections 2 through 6.
// Each case starts from an admitted document and checks its specific reason.
func refusalKeepCases() []keepRefusalCase {
	return []keepRefusalCase{
		{"new-oberth-label", "metadata", "  labels: {oberth.ci/run: forged}\n", "metadata.labels"},
		{"new-argo-label", "metadata", "  labels: {workflows.argoproj.io/controller-instanceid: forged}\n", "metadata.labels"},
		{"new-oberth-annotation", "metadata", "  annotations: {oberth.ci/spec-identity: forged}\n", "metadata.annotations"},
		{"new-argo-annotation", "metadata", "  annotations: {workflows.argoproj.io/controller-instanceid: forged}\n", "metadata.annotations"},
		{"new-unknown-annotation", "metadata", "  annotations: {oberth.ci/unknown-setting: forged}\n", "metadata.annotations"},
		{"8", "metadata", "  name: chosen\n", "metadata.name"},
		{"10", "metadata", "  namespace: other\n", "metadata.namespace"},
		{"11", "metadata", "  ownerReferences: [{apiVersion: v1, kind: Pod, name: other, uid: other}]\n", "metadata.ownerReferences"},
		{"12", "metadata", "  finalizers: [example.test/keep]\n", "metadata.finalizers"},
		{"17", "spec", "  workflowTemplateRef: {name: outside}\n", "spec.workflowTemplateRef"},
		{"18", "spec", "  podSpecPatch: '{\"serviceAccountName\":\"other\"}'\n", "spec.podSpecPatch"},
		{"19", "spec", "  artifactGC:\n    podMetadata:\n      annotations: {example.test/inject: enabled}\n", "spec.artifactGC.podMetadata.annotations"},
		{"21", "spec", "  artifactRepositoryRef: {configMap: credentials, key: store}\n", "spec.artifactRepositoryRef"},
		{"22", "spec", "  hostNetwork: true\n", "spec.hostNetwork"},
		{"24", "spec", "  hostAliases: [{ip: 127.0.0.1, hostnames: [openbao]}]\n", "spec.hostAliases"},
		{"25", "spec", "  dnsConfig: {nameservers: [127.0.0.1]}\n", "spec.dnsConfig"},
		{"26", "spec", "  schedulerName: alternate\n", "spec.schedulerName"},
		{"29", "spec", "  workflowMetadata:\n    labels: {workflows.argoproj.io/controller-instanceid: other}\n", "spec.workflowMetadata.labels"},
		{"30", "spec", "  podMetadata:\n    annotations: {example.test/inject: enabled}\n", "spec.podMetadata.annotations"},
		{"31", "spec", "  podMetadata:\n    labels: {app.kubernetes.io/instance: other}\n", "reserved"},
		{"32", "spec", "  synchronization:\n    semaphores:\n    - configMapKeyRef: {name: other, key: private}\n", "oberth-sync-limits"},
		{"35", "spec", "  volumeClaimTemplates:\n  - metadata: {name: src}\n", "server-owned claim name"},
		{"36", "spec", "  volumeClaimTemplates:\n  - metadata: {name: data}\n    spec: {volumeName: other}\n", "binds an existing volume"},
		{"37", "spec", "  volumeClaimTemplates:\n  - metadata: {name: data, finalizers: [example.test/keep]}\n", "metadata.finalizers"},
		{"39", "spec", "  arguments:\n    parameters:\n    - name: private\n      valueFrom:\n        configMapKeyRef: {name: other, key: private}\n", "valueFrom.configMapKeyRef"},
		{"40", "spec", "  arguments:\n    parameters: [{name: oberth-trigger, value: other}]\n", "server-owned"},
		{"41", "spec", "  arguments:\n    parameters: [{name: oberth-schedule, value: other}]\n", "reserved and server-owned"},
		{"42", "spec", "  hooks:\n    exit:\n      templateRef: {name: other, template: run}\n", "spec.hooks"},
		{"45", "spec", "  volumes:\n  - name: other\n    persistentVolumeClaim: {claimName: other}\n", "existing PersistentVolumeClaim"},
		{"46", "spec", "  volumes:\n  - name: private\n    projected:\n      sources:\n      - serviceAccountToken: {path: token}\n", "projects credential material"},
		{"47", "spec", "  volumes:\n  - name: host\n    hostPath: {path: /}\n", "node filesystem"},
		{"48", "spec", "  volumes:\n  - name: downward\n    downwardAPI: {}\n", "unsupported volume source"},
		{"53", "template", "      dag:\n        tasks:\n        - name: other\n          templateRef: {name: other, template: run}\n", "templateRef"},
		{"55", "deep", "", "nests inline templates deeper"},
		{"57", "template", "      resource: {action: create, manifest: \"kind: Pod\"}\n", ".resource"},
		{"58", "template", "      plugin: {other: {}}\n", ".plugin"},
		{"60", "template", "      data:\n        source:\n          artifactPaths: {name: input, s3: {bucket: other}}\n        transformation: []\n", ".data"},
		{"61", "template", "      archiveLocation: {archiveLogs: true}\n", ".archiveLocation"},
		{"62", "template", "      memoize:\n        key: poisoned\n        cache:\n          configMap: {name: shared}\n", ".memoize"},
		{"63", "template", "      metadata:\n        annotations: {example.test/inject: enabled}\n", ".metadata.annotations"},
		{"72", "container", "        envFrom:\n        - secretRef: {name: private}\n", ".envFrom"},
		{"73", "container", "        env:\n        - name: NODE\n          valueFrom:\n            fieldRef: {fieldPath: spec.nodeName}\n", "pod/downward state"},
		{"74", "container", "        env: [{name: ARGO_CONTAINER_NAME, value: wait}]\n", "reserved for the Argo executor"},
		{"75", "container", "        ports: [{containerPort: 8080, hostPort: 8080}]\n", ".hostPort"},
		{"77", "container", "        resources:\n          claims: [{name: device}]\n", ".resources.claims"},
		{"79", "container", "        volumeMounts: [{name: shadow, mountPath: /run/oberth}]\n", "reserved server helper path"},
		{"79-relative", "container", "        volumeMounts: [{name: shadow, mountPath: run/oberth/bin}]\n", "reserved server helper path"},
		{"80-mount", "container", "        volumeMounts: [{name: data, mountPath: relative/path}]\n", "must be an absolute path"},
		{"80-device", "container", "        volumeDevices: [{name: data, devicePath: relative/path}]\n", "must be an absolute path"},
	}
}

func TestRefusalKeepRows(t *testing.T) {
	for _, test := range refusalKeepCases() {
		t.Run("row-"+test.row, func(t *testing.T) {
			document := admissibleDocument
			switch test.section {
			case "metadata":
				document = strings.Replace(document, "spec:\n", "metadata:\n"+test.insert+"spec:\n", 1)
			case "spec":
				document = strings.Replace(document, "  entrypoint: main\n", "  entrypoint: main\n"+test.insert, 1)
			case "template":
				document = strings.Replace(document, "    - name: main\n", "    - name: main\n"+test.insert, 1)
			case "container":
				document = strings.Replace(document, "        command: [/bin/true]\n", "        command: [/bin/true]\n"+test.insert, 1)
			}
			workflow, err := Decode([]byte(document))
			if err != nil {
				t.Fatalf("hostile fixture must decode: %v", err)
			}
			if test.section == "deep" {
				node := workflow.Spec.Templates[0]
				for depth := 0; depth < MaxIdentityWalkDepth+2; depth++ {
					child := node
					node = wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "nested", Inline: &child}}}}
				}
				node.Name = "main"
				workflow.Spec.Templates[0] = node
			}
			err = Admit(workflow, Policy{})
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("row %s must refuse %q: %v", test.row, test.reason, err)
			}
		})
	}
}
