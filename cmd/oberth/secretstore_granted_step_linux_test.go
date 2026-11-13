package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Exercise Build -> delivered settings -> actual exec/login/fetch/revoke ->
// materialized files and a child process. Only filesystem mount locations are
// mapped to private fixtures; the TLS store and credential chain are real code.
func TestGrantedStepSecretstoreExecEndToEnd(t *testing.T) {
	store := newMockStore(t)
	ca, err := os.ReadFile(store.caPath)
	if err != nil {
		t.Fatal(err)
	}
	source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/data/r2-upload
spec:
  entrypoint: granted
  templates:
  - name: granted
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/run/oberth/bin/oberth, secretstore, exec]
      args: [--path=oberth/data/r2-upload, --, /bin/true]
`
	wf, err := argojob.Build(argojob.Config{
		Namespace: "test", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "login", CISecretsServiceAccount: "ci-login", ExecutorServiceAccount: "executor",
		PerStepIdentities: map[string]argojob.PerRepoIdentityConfig{"git/owner/repo/.oberth/release.yaml/*": {ServiceAccountName: "oberth-ci"}},
		VaultAddress:      store.URL, VaultCredentialedRole: "oberth-ci", VaultCISecretsRole: "ci-login", VaultCACertPEM: string(ca),
	}, argojob.Request{
		RunID: "granted-run", Name: "granted-run", Repo: "repo", UpstreamName: "git", UpstreamOrg: "owner", Ref: "refs/tags/v1", SHA: strings.Repeat("a", 40), Trigger: pipeline.TriggerRelease, Source: []byte(source),
		ApprovedSecrets: map[string]bool{"oberth/data/r2-upload": true}, StepGrants: map[string]map[string]bool{"oberth/data/r2-upload": {"*": true}},
		SourceVolume: argojob.SourceVolume{ClaimName: "source", SubPath: "src", VaultCASubPath: "ca", BinarySubPath: "bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	main := wf.Spec.Templates[0].Container
	tokenMounted := false
	for _, m := range main.VolumeMounts {
		tokenMounted = tokenMounted || m.Name == argojob.ReleaseTokenVolumeName
	}
	if !tokenMounted {
		t.Fatal("granted step lacks login token")
	}
	for _, env := range main.Env {
		switch env.Name {
		case "VAULT_ADDR":
			if env.Value != store.URL || env.ValueFrom != nil {
				t.Fatal("address is not the literal public store URL")
			}
			t.Setenv(env.Name, store.URL)
		case "VAULT_CACERT":
			t.Setenv(env.Name, store.caPath)
		default:
			t.Setenv(env.Name, env.Value)
		}
	}
	original := newSecretExecClient
	newSecretExecClient = func(config secretstore.Config) (*secretstore.Client, error) {
		config.ServiceAccountTokenPath = store.tokenPath
		return secretstore.New(config)
	}
	t.Cleanup(func() { newSecretExecClient = original })
	dir, root := newSecretExecTmpfsRoot(t)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runSecretStoreExec(t.Context(), []string{"--dir=" + dir, "--path=oberth/data/r2-upload", "--", "/bin/sh", "-ec", `test "$(cat "$OBERTH_SECRETSTORE_DIR/r2-upload/access")" = r2-access; test "$(cat "$OBERTH_SECRETSTORE_DIR/r2-upload/secret")" = r2-secret; test -z "${VAULT_ADDR:-}${VAULT_CACERT:-}${OBERTH_VAULT_ROLE:-}"; echo granted-child-ok`}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("exec: %v\n%s", err, stderr.String())
	}
	if stdout.String() != "granted-child-ok\n" || store.logins.Load() != 1 || store.revokes.Load() != 1 {
		t.Fatalf("out=%q logins=%d revokes=%d", stdout.String(), store.logins.Load(), store.revokes.Load())
	}
}
