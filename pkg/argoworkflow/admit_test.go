package argoworkflow

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// admissibleDocument is the smallest document that satisfies every structural
// requirement. Rejection cases below are this document plus one construct.
const admissibleDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func mustAdmit(t *testing.T, document string, policy Policy) {
	t.Helper()
	workflow, err := Decode([]byte(document))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := Admit(workflow, policy); err != nil {
		t.Fatalf("expected admission, got: %v", err)
	}
}

func refuse(t *testing.T, document string, policy Policy, expect string) {
	t.Helper()
	workflow, err := Decode([]byte(document))
	if err != nil {
		if !strings.Contains(err.Error(), expect) {
			t.Fatalf("decode error %q does not mention %q", err, expect)
		}
		return
	}
	err = Admit(workflow, policy)
	if err == nil {
		t.Fatalf("expected refusal mentioning %q, got admission", expect)
	}
	if !strings.Contains(err.Error(), expect) {
		t.Fatalf("refusal %q does not mention %q", err, expect)
	}
}

func TestAdmitAcceptsTheMinimalDocument(t *testing.T) {
	mustAdmit(t, admissibleDocument, Policy{})
}

func TestAdmitAcceptsAPipelineShapedDocument(t *testing.T) {
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: L
spec:
  entrypoint: main
  activeDeadlineSeconds: 7200
  volumeClaimTemplates:
    - metadata:
        name: work
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 8Gi
  synchronization:
    mutexes:
      - name: chart-index
  templates:
    - name: main
      dag:
        tasks:
          - name: setup
            template: run
          - name: lint
            depends: setup
            template: run
          - name: test
            depends: "lint && setup"
            template: run
    - name: run
      retryStrategy:
        limit: "2"
        backoff:
          duration: "5"
          factor: "2"
      container:
        image: golang:1.26.6-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3
        command: [/usr/local/go/bin/go, version]
        resources:
          requests:
            cpu: "2"
            memory: 4Gi
        volumeMounts:
          - name: work
            mountPath: /work
`
	mustAdmit(t, document, Policy{})
}

func TestAdmitRefusesIdentityBypassConstructs(t *testing.T) {
	cases := map[string]struct {
		insert string
		expect string
	}{
		"workflow spec podSpecPatch": {
			insert: "  podSpecPatch: '{\"serviceAccountName\":\"oberth-argo-release\"}'\n",
			expect: "spec.podSpecPatch",
		},
		"workflowTemplateRef": {
			insert: "  workflowTemplateRef:\n    name: somebody-elses-template\n",
			expect: "spec.workflowTemplateRef",
		},
		"imagePullSecrets": {
			insert: "  imagePullSecrets:\n    - name: registry-credentials\n",
			expect: "spec.imagePullSecrets",
		},
		"artifactRepositoryRef": {
			insert: "  artifactRepositoryRef:\n    configMap: artifact-repositories\n    key: default\n",
			expect: "spec.artifactRepositoryRef",
		},
		"hostNetwork": {
			insert: "  hostNetwork: true\n",
			expect: "spec.hostNetwork",
		},
		"hostAliases": {
			insert: "  hostAliases:\n    - ip: 127.0.0.1\n      hostnames: [vault]\n",
			expect: "spec.hostAliases",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleDocument, "  entrypoint: main\n",
				"  entrypoint: main\n"+testCase.insert, 1)
			refuse(t, document, Policy{}, testCase.expect)
		})
	}
}

func TestAdmitRefusesTemplateLevelBypassConstructs(t *testing.T) {
	cases := map[string]struct {
		insert string
		expect string
	}{
		"template podSpecPatch": {
			insert: "      podSpecPatch: '{\"serviceAccountName\":\"oberth-argo-release\"}'\n",
			expect: "podSpecPatch",
		},
		"resource template": {
			insert: "      resource:\n        action: create\n        manifest: |\n          apiVersion: v1\n          kind: Pod\n",
			expect: ".resource",
		},
		"http template": {
			insert: "      http:\n        url: https://example.invalid\n",
			expect: ".http",
		},
		"input artifacts": {
			insert: "      inputs:\n        artifacts:\n          - name: in\n            path: /in\n",
			expect: "artifacts",
		},
		"secret volume": {
			insert: "      volumes:\n        - name: creds\n          secret:\n            secretName: release\n",
			expect: "projects credential material",
		},
		"hostPath volume": {
			insert: "      volumes:\n        - name: node\n          hostPath:\n            path: /\n",
			expect: "mounts the node filesystem",
		},
		"existing PVC": {
			insert: "      volumes:\n        - name: shared\n          persistentVolumeClaim:\n            claimName: oberth-data\n",
			expect: "references an existing PersistentVolumeClaim",
		},
		"projected token": {
			insert: "      volumes:\n        - name: token\n          projected:\n            sources:\n              - serviceAccountToken:\n                  path: token\n",
			expect: "projects credential material",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleDocument, "    - name: main\n",
				"    - name: main\n"+testCase.insert, 1)
			refuse(t, document, Policy{}, testCase.expect)
		})
	}
}

func TestAdmitRefusesContainerCredentialReads(t *testing.T) {
	cases := map[string]struct {
		insert string
		expect string
	}{
		"envFrom secretRef": {
			insert: "        envFrom:\n          - secretRef:\n              name: release\n",
			expect: "envFrom",
		},
		"env secretKeyRef": {
			insert: "        env:\n          - name: TOKEN\n            valueFrom:\n              secretKeyRef:\n                name: release\n                key: token\n",
			expect: "reads a Kubernetes Secret",
		},
		// These three previously expected field-specific refusals. They now
		// get the blanket one: the engine forces the whole container security
		// baseline, so any securityContext a document sets is refused rather
		// than inspected field by field. The assertions moved with the
		// behaviour -- the constructs are still refused, and now so are the
		// ones the field-by-field version missed (seccompProfile, runAsUser),
		// which internal/argojob/security_test.go covers directly.
		"privileged": {
			insert: "        securityContext:\n          privileged: true\n",
			expect: "securityContext is assigned by Oberth",
		},
		"privilege escalation": {
			insert: "        securityContext:\n          allowPrivilegeEscalation: true\n",
			expect: "securityContext is assigned by Oberth",
		},
		"added capabilities": {
			insert: "        securityContext:\n          capabilities:\n            add: [SYS_ADMIN]\n",
			expect: "securityContext is assigned by Oberth",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
				"        command: [/bin/true]\n"+testCase.insert, 1)
			refuse(t, document, Policy{}, testCase.expect)
		})
	}
}

// TestAdmitRefusesDownwardAPIEnvProjection covers the deny-by-default gap
// where fieldRef and resourceFieldRef were not refused by admitContainer.
// A repository-authored pipeline must not project spec.nodeName,
// status.hostIP, or resource limits into its environment.
func TestAdmitRefusesDownwardAPIEnvProjection(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		insert string
		expect string
	}{
		"container fieldRef": {
			insert: "        env:\n          - name: NODE\n            valueFrom:\n              fieldRef:\n                fieldPath: spec.nodeName\n",
			expect: "reads pod/downward state",
		},
		"container resourceFieldRef": {
			insert: "        env:\n          - name: CPU_LIMIT\n            valueFrom:\n              resourceFieldRef:\n                resource: limits.cpu\n",
			expect: "reads pod/downward state",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
				"        command: [/bin/true]\n"+testCase.insert, 1)
			refuse(t, document, Policy{}, testCase.expect)
		})
	}
}

// TestAdmitRefusesDownwardAPIAcrossTemplateKinds ensures fieldRef/resourceFieldRef
// refusal applies to initContainers, sidecars, and script templates.
func TestAdmitRefusesDownwardAPIAcrossTemplateKinds(t *testing.T) {
	t.Parallel()
	const admissibleImage = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := map[string]string{
		"initContainer fieldRef": `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      initContainers:
        - name: init
          image: ` + admissibleImage + `
          command: [/bin/true]
          env:
            - name: HOST_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.hostIP
      container:
        image: ` + admissibleImage + `
        command: [/bin/true]
`,
		"sidecar resourceFieldRef": `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      sidecars:
        - name: side
          image: ` + admissibleImage + `
          command: [/bin/true]
          env:
            - name: MEM_LIMIT
              valueFrom:
                resourceFieldRef:
                  resource: limits.memory
      container:
        image: ` + admissibleImage + `
        command: [/bin/true]
`,
		"script fieldRef": `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      script:
        image: ` + admissibleImage + `
        command: [/bin/sh]
        source: echo hello
        env:
          - name: SA_NAME
            valueFrom:
              fieldRef:
                fieldPath: spec.serviceAccountName
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			refuse(t, document, Policy{}, "reads pod/downward state")
		})
	}
}

// TestAdmitRefusesBypassInsideInlineTemplates proves the gate recurses. An
// inline template is a complete Template value nested inside a step or DAG
// task, so a gate that only walked spec.templates would miss it entirely.
func TestAdmitRefusesBypassInsideInlineTemplates(t *testing.T) {
	const nested = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  templates:
    - name: main
      dag:
        tasks:
          - name: sneaky
            inline:
              name: inline
              podSpecPatch: '{"serviceAccountName":"oberth-argo-release"}'
              container:
                image: golang:1.26-alpine
                command: [/bin/true]
`
	refuse(t, nested, Policy{}, "podSpecPatch")
}

func TestAdmitRefusesInlineTemplateWithDeniedImage(t *testing.T) {
	const nested = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  templates:
    - name: main
      steps:
        - - name: sneaky
            inline:
              name: inline
              container:
                image: docker.io/library/alpine:3.20@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
                command: [/bin/true]
`
	refuse(t, nested, Policy{}, "outside the administrator prefix allowlist")
}

func TestAdmitRefusesExternalTemplateReferences(t *testing.T) {
	const external = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  templates:
    - name: main
      dag:
        tasks:
          - name: borrowed
            templateRef:
              name: someone-elses
              template: build
`
	refuse(t, external, Policy{}, "templateRef")
}

func TestAdmitEnforcesTheRunnerImageAllowlist(t *testing.T) {
	const admissibleImage = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreignImage = "docker.io/library/alpine:3.20@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	document := strings.Replace(admissibleDocument, admissibleImage, foreignImage, 1)
	refuse(t, document, Policy{}, "outside the administrator prefix allowlist")
	mustAdmit(t, document, Policy{RunnerImagePrefixes: []string{"docker.io/library/alpine:"}})
}

func TestAdmitRefusesMutableAndUnpinnedImages(t *testing.T) {
	const admissibleImage = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// Tag-only references are rejected (no digest)
	refuse(t, strings.Replace(admissibleDocument, admissibleImage, "golang:1.26-alpine", 1), Policy{}, "must contain an @sha256")
	refuse(t, strings.Replace(admissibleDocument, admissibleImage, "golang:latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1), Policy{}, "latest")
	refuse(t, strings.Replace(admissibleDocument, admissibleImage, "golang", 1), Policy{}, "must contain an @sha256")
}

func TestAdmitRequiresAWorkflowDeadline(t *testing.T) {
	refuse(t, strings.Replace(admissibleDocument, "  activeDeadlineSeconds: 3600\n", "", 1),
		Policy{}, "activeDeadlineSeconds")
	refuse(t, strings.Replace(admissibleDocument, "activeDeadlineSeconds: 3600", "activeDeadlineSeconds: 0", 1),
		Policy{}, "activeDeadlineSeconds")
}

func TestAdmitRefusesServerOwnedMetadata(t *testing.T) {
	for _, field := range []string{"name: chosen-by-the-repository", "generateName: chosen-", "namespace: oberth"} {
		document := strings.Replace(admissibleDocument, "kind: Workflow\n",
			"kind: Workflow\nmetadata:\n  "+field+"\n", 1)
		refuse(t, document, Policy{}, "assigned by Oberth")
	}
}

func TestAdmitRefusesAnUnknownEntrypoint(t *testing.T) {
	refuse(t, strings.Replace(admissibleDocument, "entrypoint: main", "entrypoint: nowhere", 1),
		Policy{}, "names no template")
}

func TestAdmitRefusesSuspend(t *testing.T) {
	refuse(t, strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n  suspend: true\n", 1),
		Policy{}, "suspend")
}

func TestAdmitRejectsAMalformedAdministratorAllowlist(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := Admit(workflow, Policy{RunnerImagePrefixes: []string{""}}); err == nil {
		t.Fatal("expected a malformed prefix allowlist to be refused")
	}
}

func TestTriggerFileMapping(t *testing.T) {
	for trigger, expected := range map[periapsis.Trigger]string{
		periapsis.TriggerCI:      BuildFile,
		periapsis.TriggerRelease: ReleaseFile,
	} {
		file, err := TriggerFile(trigger)
		if err != nil {
			t.Fatalf("TriggerFile(%s): %v", trigger, err)
		}
		if file != expected {
			t.Fatalf("TriggerFile(%s) = %q, want %q", trigger, file, expected)
		}
		back, ok := FileTrigger(file)
		if !ok || back != trigger {
			t.Fatalf("FileTrigger(%q) = %q,%v", file, back, ok)
		}
	}
	if _, err := TriggerFile(periapsis.Trigger("unknown")); err == nil {
		t.Fatal("expected unknown trigger to have no Argo-authored pipeline file")
	}
}

// A hostPort walks straight past every other node-isolation denial: hostNetwork,
// hostAliases, hostPath, nodeSelector and affinity were all refused while this
// bound an arbitrary port on the shared node. 30022 is Oberth's own Git ingress,
// and whichever of the two bound first would own it.
func TestAdmitRejectsHostPort(t *testing.T) {
	t.Parallel()
	refuse(t, admissibleDocument+`        ports:
          - containerPort: 8080
            hostPort: 30022
`, Policy{}, "hostPort")
}

// hostIP addresses the node just as directly as hostPort does.
func TestAdmitRejectsHostIP(t *testing.T) {
	t.Parallel()
	refuse(t, admissibleDocument+`        ports:
          - containerPort: 8080
            hostIP: 10.149.6.32
`, Policy{}, "hostIP")
}

// containerPort on its own is pod-local and grants nothing outside the Pod's
// network namespace: a daemon template declares it so a sibling step can reach
// it. Refusing it would break a legitimate pattern for no gain.
func TestAdmitAllowsPodLocalContainerPort(t *testing.T) {
	t.Parallel()
	mustAdmit(t, admissibleDocument+`        ports:
          - containerPort: 8080
`, Policy{})
}

func TestAdmitRejectsVCTNamedSrc(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: src
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "collides with a server-owned claim name")
}

func TestAdmitAcceptsVCTNamedWork(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: work
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

func TestAdmitRejectsVCTDataSource(t *testing.T) {
	t.Parallel()
	refuse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: workspace
      spec:
        accessModes: [ReadWriteOnce]
        dataSource:
          kind: PersistentVolumeClaim
          name: stolen-source
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`, Policy{}, "dataSource can clone another run")
}

func TestAdmitRejectsVCTDataSourceRef(t *testing.T) {
	t.Parallel()
	refuse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: workspace
      spec:
        accessModes: [ReadWriteOnce]
        dataSourceRef:
          kind: PersistentVolumeClaim
          name: stolen-source
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`, Policy{}, "dataSourceRef can clone another run")
}

func TestAdmitRejectsVCTSelector(t *testing.T) {
	t.Parallel()
	refuse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: workspace
      spec:
        accessModes: [ReadWriteOnce]
        selector:
          matchLabels:
            app: stolen
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`, Policy{}, "selector can bind an existing volume")
}

func TestAdmitRejectsVCTFinalizers(t *testing.T) {
	t.Parallel()
	refuse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: workspace
        finalizers: ["kubernetes.io/pvc-protection"]
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`, Policy{}, "finalizers must be omitted")
}

func TestAdmitRejectsVCTOwnerReferences(t *testing.T) {
	t.Parallel()
	refuse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: workspace
        ownerReferences:
          - apiVersion: v1
            kind: Pod
            name: steal
            uid: aaaa
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`, Policy{}, "ownerReferences must be omitted")
}

func TestAdmitRejectsUnresolvedOnExit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  onExit: cleanup
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "spec.onExit references template \"cleanup\"")
}

func TestAdmitRejectsUnresolvedInlineDAGReference(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      dag:
        tasks:
          - name: outer
            inline:
              dag:
                tasks:
                  - name: inner
                    template: nonexistent
              name: nested
    - name: unused
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "references template \"nonexistent\"")
}

func TestAdmitRejectsUnresolvedInlineStepReference(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      steps:
        - - name: outer
            inline:
              steps:
                - - name: inner
                    template: missing
              name: nested
    - name: unused
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "references template \"missing\"")
}

// --- Finding 7 tests: resource amplification ceilings ---

func TestAdmitRefusesExcessiveRetryLimit(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "      container:", "      retryStrategy:\n        limit: \"99\"\n      container:", 1)
	refuse(t, document, Policy{}, "retryStrategy.limit 99 exceeds")
}

func TestAdmitAcceptsRetryLimitAtCeiling(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "      container:", "      retryStrategy:\n        limit: \"10\"\n      container:", 1)
	mustAdmit(t, document, Policy{})
}

func TestAdmitRefusesExcessiveCPU(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
		"        command: [/bin/true]\n        resources:\n          requests:\n            cpu: \"32\"\n", 1)
	refuse(t, document, Policy{}, "cpu 32 exceeds the 8 ceiling")
}

func TestAdmitAcceptsCPUAtCeiling(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
		"        command: [/bin/true]\n        resources:\n          requests:\n            cpu: \"8\"\n            memory: 16Gi\n", 1)
	mustAdmit(t, document, Policy{})
}

func TestAdmitRefusesExcessiveMemory(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
		"        command: [/bin/true]\n        resources:\n          requests:\n            memory: 128Gi\n", 1)
	refuse(t, document, Policy{}, "memory 128Gi exceeds the 16Gi ceiling")
}

func TestAdmitRefusesExcessivePVCCapacity(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 256Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "requests 256Gi storage, maximum is 64Gi")
}

func TestAdmitAcceptsPVCCapacityAtCeiling(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 64Gi
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

func TestAdmitRefusesMemoryEmptyDirWithoutSizeLimit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      volumes:
        - name: ramdisk
          emptyDir:
            medium: Memory
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "memory-backed emptyDir without a sizeLimit")
}

func TestAdmitAcceptsMemoryEmptyDirWithBoundedSizeLimit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      volumes:
        - name: secrets
          emptyDir:
            medium: Memory
            sizeLimit: 1Mi
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

// Issue #440: disk-backed emptyDirs must carry a bounded sizeLimit, just
// like memory-backed ones. An uncapped disk-backed emptyDir can exhaust the
// node's ephemeral storage.
func TestAdmitRefusesDiskEmptyDirWithoutSizeLimit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      volumes:
        - name: scratch
          emptyDir: {}
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "disk-backed emptyDir without a sizeLimit")
}

func TestAdmitAcceptsDiskEmptyDirWithBoundedSizeLimit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      volumes:
        - name: scratch
          emptyDir:
            sizeLimit: 4Gi
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

func TestAdmitRefusesDiskEmptyDirExceedingCeiling(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      volumes:
        - name: scratch
          emptyDir:
            sizeLimit: 100Gi
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "exceeds the 50Gi ceiling")
}

func TestAdmitRefusesGPUResources(t *testing.T) {
	t.Parallel()
	document := strings.Replace(admissibleDocument, "        command: [/bin/true]\n",
		"        command: [/bin/true]\n        resources:\n          requests:\n            nvidia.com/gpu: \"1\"\n", 1)
	refuse(t, document, Policy{}, "not admitted")
}

// --- Finding 8 tests: synchronization lock scoping ---

func TestAdmitRefusesConfigMapSemaphore(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  synchronization:
    semaphores:
      - configMapKeyRef:
          name: my-config
          key: limit
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "configMapKeyRef reads a Kubernetes ConfigMap")
}

func TestAdmitRefusesDatabaseMutex(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  synchronization:
    mutexes:
      - name: my-lock
        database: true
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "database is not admitted")
}

func TestAdmitRefusesMutexNamespaceOverride(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  synchronization:
    mutexes:
      - name: my-lock
        namespace: kube-system
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "namespace overrides are not admitted")
}

func TestAdmitAcceptsNamedMutex(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  synchronization:
    mutexes:
      - name: chart-index
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

// TestOberthPipelinesPassAdmission proves Oberth's own pipeline documents
// pass the admission gate including all newly added ceilings. The policy
// includes the image prefixes the live deployment allowlists.
func TestOberthPipelinesPassAdmission(t *testing.T) {
	t.Parallel()
	policy := Policy{RunnerImagePrefixes: periapsis.DefaultRunnerImagePrefixes}
	for _, file := range []string{"../../.oberth/build.yaml", "../../.oberth/release.yaml"} {
		name := file
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Skipf("pipeline file not found: %s", file)
			}
			workflow, err := Decode(source)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := Admit(workflow, policy); err != nil {
				t.Fatalf("admission: %v", err)
			}
		})
	}
}

func TestAdmitAcceptsResolvedOnExit(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  onExit: cleanup
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
    - name: cleanup
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}

// TestAdmitRefusesMixedDependsAndDependencies checks that a DAG template
// mixing enhanced-depends (string expressions) with legacy dependencies
// (string arrays) is rejected at admission, matching the executor's own check.
func TestAdmitRefusesMixedDependsAndDependencies(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: pipeline
  activeDeadlineSeconds: 3600
  templates:
    - name: pipeline
      dag:
        tasks:
          - name: setup
            template: work
          - name: lint
            depends: setup
            template: work
          - name: test
            dependencies: [setup]
            template: work
    - name: work
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	refuse(t, document, Policy{}, "cannot use both 'depends' and 'dependencies'")
}

// TestAdmitAcceptsConsistentDependsOnly verifies that a DAG using only the
// enhanced depends syntax passes admission.
func TestAdmitAcceptsConsistentDependsOnly(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: pipeline
  activeDeadlineSeconds: 3600
  templates:
    - name: pipeline
      dag:
        tasks:
          - name: setup
            template: work
          - name: lint
            depends: setup
            template: work
          - name: test
            depends: setup
            template: work
    - name: work
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	mustAdmit(t, document, Policy{})
}
