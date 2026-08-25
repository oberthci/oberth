package argojob

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// These tests cover the gap that an audit test was reporting as closed: the
// Argo path set no securityContext at all, and admission only refused an
// explicitly-set privileged:true. A document declaring seccompProfile:
// Unconfined or runAsUser ran exactly as written, on both tiers, on the same
// node as the Oberth server.

const nestedSecurityDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      dag:
        tasks:
          - name: inline-task
            inline:
              name: inline
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
    - name: sidecar-carrier
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
      initContainers:
        - name: init
          image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          command: [/bin/true]
      sidecars:
        - name: side
          image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          command: [/bin/true]
`

// TestBuildForcesThePodSecurityBaseline proves the workflow-level context is
// server-assigned rather than absent.
func TestBuildForcesThePodSecurityBaseline(t *testing.T) {
	built, err := Build(testConfig(), testRequest(periapsis.TriggerCI, nestedSecurityDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	security := built.Spec.SecurityContext
	if security == nil {
		t.Fatal("no Pod security context was forced")
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp profile = %+v, want RuntimeDefault", security.SeccompProfile)
	}
	// Parity with internal/job/spec.go, which runs as uid 0 with
	// RunAsNonRoot false. Tightening both paths to a non-root UID is a
	// separate change; diverging here would mean a pipeline gains or loses
	// privilege purely by changing authoring format.
	if security.RunAsUser == nil || *security.RunAsUser != 0 {
		t.Fatalf("runAsUser = %v, want 0 (parity with the Job engine)", security.RunAsUser)
	}
	if security.RunAsNonRoot == nil || *security.RunAsNonRoot {
		t.Fatalf("runAsNonRoot = %v, want false (parity with the Job engine)", security.RunAsNonRoot)
	}
}

// TestBuildForcesTheContainerSecurityBaselineEverywhere walks every container
// position, including inline templates, init containers and sidecars.
func TestBuildForcesTheContainerSecurityBaselineEverywhere(t *testing.T) {
	built, err := Build(testConfig(), testRequest(periapsis.TriggerCI, nestedSecurityDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	checked := 0
	check := func(where string, container *corev1.Container) {
		if container == nil {
			return
		}
		checked++
		security := container.SecurityContext
		if security == nil {
			t.Errorf("%s has no forced security context", where)
			return
		}
		if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
			t.Errorf("%s allows privilege escalation", where)
		}
		if security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
			t.Errorf("%s does not have a read-only root filesystem", where)
		}
		if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s does not drop all capabilities", where)
		}
		if security.Capabilities != nil && len(security.Capabilities.Add) > 0 {
			t.Errorf("%s adds capabilities: %v", where, security.Capabilities.Add)
		}
		// A read-only root is only safe because /tmp is writable: the Go
		// toolchain writes to os.TempDir() on every build.
		writableTmp := false
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == stepTmpMountPath && !mount.ReadOnly {
				writableTmp = true
			}
		}
		if !writableTmp {
			t.Errorf("%s has a read-only root filesystem and no writable %s", where, stepTmpMountPath)
		}
	}
	for index := range built.Spec.Templates {
		template := &built.Spec.Templates[index]
		check("template "+template.Name, template.Container)
		if template.SecurityContext != nil {
			t.Errorf("template %s retained a template-level Pod security context", template.Name)
		}
		for inner := range template.InitContainers {
			check("initContainer in "+template.Name, &template.InitContainers[inner].Container)
		}
		for inner := range template.Sidecars {
			check("sidecar in "+template.Name, &template.Sidecars[inner].Container)
		}
		if template.DAG != nil {
			for _, task := range template.DAG.Tasks {
				if task.Inline != nil {
					check("inline template in "+template.Name, task.Inline.Container)
				}
			}
		}
	}
	if checked < 4 {
		t.Fatalf("only %d containers were checked; the fixture should cover container, init, sidecar and inline", checked)
	}
}

func TestBuildProvidesAWritableTmpVolume(t *testing.T) {
	built, err := Build(testConfig(), testRequest(periapsis.TriggerCI, nestedSecurityDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := false
	for _, volume := range built.Spec.Volumes {
		if volume.Name != stepTmpVolumeName {
			continue
		}
		found = true
		if volume.EmptyDir == nil {
			t.Fatalf("%s is not an emptyDir: %+v", stepTmpVolumeName, volume)
		}
	}
	if !found {
		t.Fatalf("no %s volume; a read-only root filesystem would break every build", stepTmpVolumeName)
	}
}

// TestAdmitRefusesSecurityContextOverrides is the half that stops the forced
// baseline being weakened. Each of these passed admission before.
func TestAdmitRefusesSecurityContextOverrides(t *testing.T) {
	cases := map[string]string{
		"pod seccomp unconfined": "  securityContext:\n    seccompProfile:\n      type: Unconfined\n",
		"pod runAsUser":          "  securityContext:\n    runAsUser: 0\n",
		"pod sysctls":            "  securityContext:\n    sysctls:\n      - name: net.core.somaxconn\n        value: \"1024\"\n",
	}
	for name, insert := range cases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleSecurityDocument, "  entrypoint: main\n",
				"  entrypoint: main\n"+insert, 1)
			workflow, err := argoworkflow.Decode([]byte(document))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err == nil {
				t.Fatal("a Pod security override was admitted")
			}
		})
	}

	containerCases := map[string]string{
		"container seccomp unconfined": "        securityContext:\n          seccompProfile:\n            type: Unconfined\n",
		"container runAsUser":          "        securityContext:\n          runAsUser: 0\n",
		"container writable root":      "        securityContext:\n          readOnlyRootFilesystem: false\n",
		"container appArmor":           "        securityContext:\n          appArmorProfile:\n            type: Unconfined\n",
	}
	for name, insert := range containerCases {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(admissibleSecurityDocument, "        command: [/bin/true]\n",
				"        command: [/bin/true]\n"+insert, 1)
			workflow, err := argoworkflow.Decode([]byte(document))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err == nil {
				t.Fatal("a container security override was admitted")
			}
		})
	}
}

// TestAdmitRefusesSecurityOverridesInsideInlineTemplates proves the refusal
// recurses, matching the forcer.
func TestAdmitRefusesSecurityOverridesInsideInlineTemplates(t *testing.T) {
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
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
                securityContext:
                  seccompProfile:
                    type: Unconfined
`
	workflow, err := argoworkflow.Decode([]byte(nested))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err == nil {
		t.Fatal("a security override inside an inline template was admitted")
	}
}

// TestOberthPipelinesCarryNoSecurityContext proves the migrated documents rely
// on the forced baseline rather than declaring their own.
func TestOberthPipelinesCarryNoSecurityContext(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		workflow, err := argoworkflow.Decode(loadPipeline(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if workflow.Spec.SecurityContext != nil {
			t.Errorf("%s declares a Pod security context", name)
		}
		for _, template := range workflow.Spec.Templates {
			if template.Container != nil && template.Container.SecurityContext != nil {
				t.Errorf("%s template %q declares a container security context", name, template.Name)
			}
		}
	}
}

const admissibleSecurityDocument = `
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

// A release that can never authenticate is refused at admission.
//
// The v0.12.4 release ran setup, lint, scan, test, build and chart-test green,
// then failed both credentialed steps three times each on
// "x509: certificate signed by unknown authority": OpenBao served a certificate
// from the installer's own private CA, the release tier was pointed at it, and
// no anchor had ever been configured, so no VAULT_CACERT reached any container.
// Nothing in the submission path had an opinion about that, so the deployment
// spent a version number and half an hour to discover a value was missing.
func TestBuildRefusesAReleaseThatCanNeverVerifyTheStore(t *testing.T) {
	t.Parallel()
	// The vault CA check only fires for credentialed runs (those with declared
	// paths). Use a document with a declared path so the release is credentialed.
	withPaths := strings.Replace(admissibleSecurityDocument, "spec:\n",
		"metadata:\n  annotations:\n    oberth.ci/secret-paths: oberth/data/release/r2-upload-token\nspec:\n", 1)
	credentialedRelease := func() Request {
		request := releaseRequestWithAnchor(withPaths)
		request.ApprovedSecrets = map[string]bool{"oberth/data/release/r2-upload-token": true}
		return request
	}
	config := testConfig()
	config.VaultCACertPEM = ""

	_, err := Build(config, credentialedRelease())
	if err == nil {
		t.Fatal("a release was admitted for a store the release tier cannot verify")
	}
	for _, want := range []string{"no publicly trusted CA may issue for", "argo.vault.caCert", config.VaultAddress} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// The same deployment must keep running branch CI. A branch push receives
	// no token and contacts no store, so refusing it would take the engine down
	// over a value only releases use.
	if _, err := Build(config, testRequest(periapsis.TriggerCI, admissibleSecurityDocument)); err != nil {
		t.Fatalf("branch CI was refused over a release-only value: %v", err)
	}

	// With the anchor configured the same release is admitted.
	if _, err := Build(testConfig(), credentialedRelease()); err != nil {
		t.Fatalf("an anchored release was refused: %v", err)
	}

	// A publicly trusted store legitimately needs no anchor, and that has to
	// keep working or the guard becomes a reason to pin a CA that is not one.
	public := testConfig()
	public.VaultCACertPEM = ""
	public.VaultAddress = "https://bao.example.com:8200"
	publicRelease := credentialedRelease()
	publicRelease.ApprovedSecrets = map[string]bool{"oberth/data/release/r2-upload-token": true}
	if _, err := Build(public, publicRelease); err != nil {
		t.Fatalf("a public store was refused for having no private anchor: %v", err)
	}
}

// The boundary is which names a public CA is permitted to issue for, not a
// guess about which ones look internal.
// TestBuildMountsReleaseTokenOnlyInCredentialedTemplates proves the release
// tier's projected ServiceAccount token is mounted only in templates whose
// container command is envconsul -- the credential chain. Non-credentialed
// templates in a release Workflow have no use for the login token and must not
// carry it: mounting it widens the blast radius without enabling anything the
// template would do with it.
func TestBuildMountsReleaseTokenOnlyInCredentialedTemplates(t *testing.T) {
	t.Parallel()
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/data/release/r2-upload-token
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      dag:
        tasks:
          - name: setup
            template: setup
          - name: publish
            template: publish
    - name: setup
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
    - name: publish
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/tmp/tools/envconsul]
        args: ["-once", "-config", "test.hcl"]
`
	request := testRequest(periapsis.TriggerRelease, document)
	request.ApprovedSecrets = map[string]bool{"oberth/data/release/r2-upload-token": true}
	request.SourceDir = writeEnvconsulWorkspace(t, map[string]string{
		"test.hcl": `secret {
  path = "oberth/data/release/r2-upload-token"
}
`,
	})
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	// The projected volume must exist at the workflow level: it is what the
	// kubelet uses to mint the token that envconsul presents to Vault.
	var projected *corev1.Volume
	for index := range workflow.Spec.Volumes {
		if workflow.Spec.Volumes[index].Name == ReleaseTokenVolumeName {
			projected = &workflow.Spec.Volumes[index]
		}
	}
	if projected == nil || projected.Projected == nil {
		t.Fatal("the release token volume is not declared at the workflow level")
	}

	checked := 0
	for _, template := range workflow.Spec.Templates {
		if template.Container == nil {
			continue
		}
		checked++
		hasToken := false
		for _, mount := range template.Container.VolumeMounts {
			if mount.Name == ReleaseTokenVolumeName || mount.MountPath == ReleaseTokenMountPath {
				hasToken = true
			}
		}
		if templateUsesEnvconsul(&template) && !hasToken {
			t.Errorf("credentialed template %q does not mount the release token", template.Name)
		}
		if !templateUsesEnvconsul(&template) && hasToken {
			t.Errorf("non-credentialed template %q mounts the release token", template.Name)
		}
	}
	if checked < 2 {
		t.Fatalf("only %d container templates checked; the fixture must have both credentialed and non-credentialed templates", checked)
	}
}

func TestVaultAddressRequiresPinnedAnchor(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]bool{
		"https://openbao.openbao.svc:8200":               true,
		"https://openbao.openbao.svc.cluster.local:8200": true,
		"https://openbao.openbao.svc.":                   true,
		"https://openbao:8200":                           true,
		"https://bao.corp.internal":                      true,
		"https://bao.home.local":                         true,
		"https://10.43.150.79:8200":                      true,
		"https://127.0.0.1:8200":                         true,
		"https://[::1]:8200":                             true,
		"https://169.254.1.1:8200":                       true,
		// Literal addresses are pinned whatever their range; see the comment
		// on vaultAddressRequiresPinnedAnchor.
		"https://8.8.8.8:8200":         true,
		"https://bao.example.com:8200": false,
		"https://vault.skipops.ltd":    false,
		"":                             false,
	} {
		if got := vaultAddressRequiresPinnedAnchor(address); got != want {
			t.Errorf("vaultAddressRequiresPinnedAnchor(%q) = %v, want %v", address, got, want)
		}
	}
}
