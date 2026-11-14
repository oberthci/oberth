package argojob

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

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
        - name: prepare
          image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          command: [/bin/true]
      sidecars:
        - name: side
          image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          command: [/bin/true]
`

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
	// The CA check applies to granted store consumers. This main step
	// declares its path and has an explicit wildcard fixture grant.
	withPaths := strings.Replace(strings.Replace(admissibleSecurityDocument, "command: [/bin/true]", "command: [oberth, secretstore, exec, --path=oberth/data/release/r2-upload-token, --, /bin/true]", 1), "spec:\n",
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
	if _, err := Build(config, testRequest(pipeline.TriggerCI, admissibleSecurityDocument)); err != nil {
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
// container command is secretstore exec -- the credential chain. Non-credentialed
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
        command: [oberth, secretstore, exec]
        args: ["--path", "oberth/data/release/r2-upload-token", "--", "/bin/true"]
    - name: ordinary-envconsul
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [envconsul]
`
	request := testRequest(pipeline.TriggerRelease, document)
	request.ApprovedSecrets = map[string]bool{"oberth/data/release/r2-upload-token": true}
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	// The projected volume must exist at the workflow level: it is what the
	// kubelet uses to mint the token that secretstore exec presents to Vault.
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
		if templateUsesOberthSecretstore(&template) && !hasToken {
			t.Errorf("credentialed template %q does not mount the release token", template.Name)
		}
		if !templateUsesOberthSecretstore(&template) && hasToken {
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
		"https://vault.acme.ltd":       false,
		"":                             false,
	} {
		if got := vaultAddressRequiresPinnedAnchor(address); got != want {
			t.Errorf("vaultAddressRequiresPinnedAnchor(%q) = %v, want %v", address, got, want)
		}
	}
}
