package argojob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// writeEnvconsulWorkspace materialises a fake immutable run workspace holding
// the given relative files, returning its root for Request.SourceDir.
func writeEnvconsulWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for relative, content := range files {
		full := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", relative, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}
	return root
}

// ciEnvconsulDocument mirrors the terraform repository's plan template: a CI
// trigger, an upstream-scoped declared path, and envconsul reading two
// repository-authored -config files with relative paths.
const ciEnvconsulDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/upstream/skipops/oberth/credentials
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["/tmp/oberth-tools/bin/envconsul"]
        args: ["-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl", "-once", "-pristine", "/bin/true"]
`

func ciEnvconsulRequest(t *testing.T, files map[string]string) Request {
	t.Helper()
	request := testRequest(periapsis.TriggerCI, ciEnvconsulDocument)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	request.SourceDir = writeEnvconsulWorkspace(t, files)
	return request
}

// vaultOnlyHCL carries no secret path at all — the connection half of the
// terraform repository's real split configuration.
const vaultOnlyHCL = `vault {
  k8s_service_account_token_path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
  renew_token = false
}
`

// TestEnvconsulAdmitsDeclaredConfigPaths proves the terraform-shaped pipeline
// — declared upstream path, HCL fetching its KV v2 API spelling — is admitted
// and runs as the ci-secrets identity.
func TestEnvconsulAdmitsDeclaredConfigPaths(t *testing.T) {
	request := ciEnvconsulRequest(t, map[string]string{
		".oberth/envconsul.hcl": vaultOnlyHCL,
		".oberth/envconsul/plan.hcl": `secret {
  no_prefix = true
  path      = "oberth/data/upstream/skipops/oberth/credentials"
}
`,
	})
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if workflow.Spec.ServiceAccountName != testCISecretsAcct {
		t.Fatalf("ServiceAccount = %q, want %q", workflow.Spec.ServiceAccountName, testCISecretsAcct)
	}
}

// TestEnvconsulAdmitsLogicalKVSpelling proves the logical (non-data) spelling
// of a declared path is also admitted: envconsul's Vault client rewrites
// logical KV v2 paths onto the data endpoint, so both spellings authorize the
// same read.
func TestEnvconsulAdmitsLogicalKVSpelling(t *testing.T) {
	request := ciEnvconsulRequest(t, map[string]string{
		".oberth/envconsul.hcl": vaultOnlyHCL,
		".oberth/envconsul/plan.hcl": `secret {
  path = "oberth/upstream/skipops/oberth/credentials"
}
`,
	})
	if _, err := Build(testConfig(), request); err != nil {
		t.Fatalf("logical spelling of the declared path was rejected: %v", err)
	}
}

// TestEnvconsulRefusesUndeclaredConfigPath is the second manifestation of
// issue #200: a `secret {}` stanza fetching a path outside the declared
// annotation must fail admission, whatever the pod's Vault policy would say.
func TestEnvconsulRefusesUndeclaredConfigPath(t *testing.T) {
	request := ciEnvconsulRequest(t, map[string]string{
		".oberth/envconsul.hcl": vaultOnlyHCL,
		".oberth/envconsul/plan.hcl": `secret {
  path = "oberth/data/upstream/skipops/oberth/credentials"
}

secret {
  path = "oberth/data/release/cosign-secret"
}
`,
	})
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("an undeclared secret stanza was admitted")
	}
	if !strings.Contains(err.Error(), "oberth/data/release/cosign-secret") {
		t.Fatalf("refusal does not name the undeclared path: %v", err)
	}
}

// TestEnvconsulRefusesUndeclaredSecretFlag covers the -secret command-line
// spelling of the same bypass.
func TestEnvconsulRefusesUndeclaredSecretFlag(t *testing.T) {
	document := strings.Replace(ciEnvconsulDocument,
		`"-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl"`,
		`"-secret=oberth/data/release/cosign-secret"`, 1)
	request := testRequest(periapsis.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("an undeclared -secret flag was admitted")
	}
	if !strings.Contains(err.Error(), "oberth/data/release/cosign-secret") {
		t.Fatalf("refusal does not name the undeclared path: %v", err)
	}
}

// TestEnvconsulAdmitsDeclaredSecretFlag proves the -secret flag is admitted
// when it names a declared path, in either spelling.
func TestEnvconsulAdmitsDeclaredSecretFlag(t *testing.T) {
	for _, spelling := range []string{
		"oberth/data/upstream/skipops/oberth/credentials",
		"oberth/upstream/skipops/oberth/credentials",
	} {
		document := strings.Replace(ciEnvconsulDocument,
			`"-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl"`,
			`"-secret", "`+spelling+`"`, 1)
		request := testRequest(periapsis.TriggerCI, document)
		request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
		if _, err := Build(testConfig(), request); err != nil {
			t.Fatalf("declared -secret %q was rejected: %v", spelling, err)
		}
	}
}

// TestEnvconsulRefusesConfigOutsideSource proves a -config path outside the
// immutable checkout is refused: /tmp is a writable emptyDir an earlier
// container of the same pod could have filled, so nothing can vouch for a
// file there.
func TestEnvconsulRefusesConfigOutsideSource(t *testing.T) {
	for _, outside := range []string{"/tmp/evil.hcl", "../evil.hcl", "/etc/envconsul.hcl"} {
		document := strings.Replace(ciEnvconsulDocument,
			`"-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl"`,
			`"-config", "`+outside+`"`, 1)
		request := testRequest(periapsis.TriggerCI, document)
		request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
		request.SourceDir = writeEnvconsulWorkspace(t, nil)
		if _, err := Build(testConfig(), request); err == nil {
			t.Fatalf("a -config outside the source checkout (%q) was admitted", outside)
		}
	}
}

// TestEnvconsulAdmitsAbsoluteSourcePath proves the /work/src-anchored absolute
// spelling of an in-checkout config resolves to the same file as the relative
// spelling and is admitted.
func TestEnvconsulAdmitsAbsoluteSourcePath(t *testing.T) {
	document := strings.Replace(ciEnvconsulDocument,
		`"-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl"`,
		`"-config", "/work/src/.oberth/envconsul/plan.hcl"`, 1)
	request := testRequest(periapsis.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	request.SourceDir = writeEnvconsulWorkspace(t, map[string]string{
		".oberth/envconsul/plan.hcl": `secret {
  path = "oberth/data/upstream/skipops/oberth/credentials"
}
`,
	})
	if _, err := Build(testConfig(), request); err != nil {
		t.Fatalf("the /work/src-anchored spelling was rejected: %v", err)
	}
}

// TestEnvconsulRefusesMissingUnparseableOrDirectoryConfig proves every shape
// the server cannot read and understand is refused rather than deferred to
// runtime: an absent file, one that does not parse, and a directory.
func TestEnvconsulRefusesMissingUnparseableOrDirectoryConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"missing": {".oberth/envconsul.hcl": vaultOnlyHCL},
		"unparseable": {
			".oberth/envconsul.hcl":      vaultOnlyHCL,
			".oberth/envconsul/plan.hcl": `secret { path = `,
		},
		"directory": {
			".oberth/envconsul.hcl":               vaultOnlyHCL,
			".oberth/envconsul/plan.hcl/real.txt": "the -config arg names the parent directory",
		},
	}
	for name, files := range cases {
		request := ciEnvconsulRequest(t, files)
		if _, err := Build(testConfig(), request); err == nil {
			t.Errorf("%s: an unverifiable envconsul config was admitted", name)
		}
	}
}

// TestEnvconsulRefusesWithoutDeclaredAnnotation mirrors the secretstore-exec
// rule: credential configuration in a workflow that declares no paths is
// refused with a clear admission error instead of a runtime login failure.
func TestEnvconsulRefusesWithoutDeclaredAnnotation(t *testing.T) {
	document := strings.Replace(ciEnvconsulDocument,
		"    oberth.ci/secret-paths: oberth/upstream/skipops/oberth/credentials\n", "", 1)
	request := testRequest(periapsis.TriggerCI, document)
	request.SourceDir = writeEnvconsulWorkspace(t, map[string]string{
		".oberth/envconsul.hcl":      vaultOnlyHCL,
		".oberth/envconsul/plan.hcl": `secret { path = "oberth/data/upstream/skipops/oberth/credentials" }`,
	})
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("envconsul credential configuration without a declared annotation was admitted")
	}
	if !strings.Contains(err.Error(), "declares no") {
		t.Fatalf("refusal does not explain the missing annotation: %v", err)
	}
}

// TestEnvconsulCheckedInsideInlineTemplates proves an inline template cannot
// smuggle an envconsul invocation past admission: the walk that injects the
// credential mount and the walk that admits the invocation cover the same
// set.
func TestEnvconsulCheckedInsideInlineTemplates(t *testing.T) {
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/upstream/skipops/oberth/credentials
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
                command: [envconsul]
                args: ["-secret", "oberth/data/release/cosign-secret", "/bin/true"]
`
	request := testRequest(periapsis.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("an inline envconsul template fetching an undeclared path was admitted")
	}
	if !strings.Contains(err.Error(), "oberth/data/release/cosign-secret") {
		t.Fatalf("refusal does not name the undeclared path: %v", err)
	}
}

// TestSecretstoreExecCheckedInsideInlineTemplates proves the --path admission
// reaches inline templates too — the same gap, same fix, for the native
// chain.
func TestSecretstoreExecCheckedInsideInlineTemplates(t *testing.T) {
	const document = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/upstream/skipops/oberth/credentials
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
	request := testRequest(periapsis.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("an inline secretstore-exec template fetching an undeclared path was admitted")
	}
	if !strings.Contains(err.Error(), "oberth/data/release/cosign-secret") {
		t.Fatalf("refusal does not name the undeclared path: %v", err)
	}
}

// TestEnvconsulWithoutCredentialConfigurationIsAdmitted proves envconsul with
// no -config and no -secret checks nothing and needs no workspace: it fetches
// nothing.
func TestEnvconsulWithoutCredentialConfigurationIsAdmitted(t *testing.T) {
	document := strings.Replace(ciEnvconsulDocument,
		`"-config", ".oberth/envconsul.hcl", "-config", ".oberth/envconsul/plan.hcl", `, "", 1)
	request := testRequest(periapsis.TriggerCI, document)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/credentials": true}
	if _, err := Build(testConfig(), request); err != nil {
		t.Fatalf("envconsul without credential configuration was rejected: %v", err)
	}
}

// TestAllowedStoreSpellings pins the declared-to-fetch path mapping the
// admission comparison rests on.
func TestAllowedStoreSpellings(t *testing.T) {
	allowed := allowedStoreSpellings([]string{
		"oberth/upstream/skipops/oberth/credentials",
		"oberth/data/release/cosign-secret",
	})
	for _, spelling := range []string{
		"oberth/upstream/skipops/oberth/credentials",
		"oberth/data/upstream/skipops/oberth/credentials",
		"oberth/data/release/cosign-secret",
		"oberth/release/cosign-secret",
	} {
		if _, ok := allowed[spelling]; !ok {
			t.Errorf("spelling %q is not covered", spelling)
		}
	}
	for _, foreign := range []string{
		"oberth/data/upstream/skipops/other/credentials",
		"oberth/data/release/gar-sa-key",
		"oberth/metadata/release/cosign-secret",
	} {
		if _, ok := allowed[foreign]; ok {
			t.Errorf("foreign path %q is covered", foreign)
		}
	}
}

// TestExtractFlagValues pins the four flag spellings envconsul accepts.
func TestExtractFlagValues(t *testing.T) {
	values := extractFlagValues(
		[]string{"/bin/envconsul"},
		[]string{"-config", "a.hcl", "--config", "b.hcl", "-config=c.hcl", "--config=d.hcl", "-once", "child", "-config-like"},
		"config")
	want := []string{"a.hcl", "b.hcl", "c.hcl", "d.hcl"}
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Fatalf("values = %v, want %v", values, want)
		}
	}
}
