//go:build linux

// These tests exercise Linux Git receive and memory-backed secret delivery.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestComposedSecretWriteBareStepAndLiveState(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	backend := f.secretBackend(t)
	f.prepareSecretCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	remote := f.administratorClient(t)
	f.writeSecretThroughCLI(t)
	f.checkReleaseSecretWrite(t, remote, backend)
	login := backend.Resources["/auth/"+f.config.SecretStore.AuthMount+"/login"]
	if login["role"] != f.config.SecretStore.WriterRole || login["jwt"] != "server-projected-token" {
		t.Fatal("server writer did not use its configured projected identity")
	}
	f.checkLiveState(t, remote)
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/secret")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	source := f.deliverSource(t, run)
	f.consumeSecretStep(t, run, source)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	if !backend.Revoked {
		t.Fatal("store session survived delivery")
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "secret.put.requested", Limit: 10})
	raw, _ := json.Marshal(rows)
	if err != nil || len(rows) != 2 || bytes.Contains(raw, []byte("fixture-secret")) {
		t.Fatal("secret write audit lost or exposed value", err)
	}
	f.checkOnce(t)
}
func (f *composedFixture) secretBackend(t *testing.T) *storefake.Store {
	t.Helper()
	backend := &storefake.Store{}
	server := deliveryHTTPServer(t, backend)
	f.config.SecretStore.Address = server.URL
	f.config.SecretStore.Audience = "composed-store-audience"
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(f.config.Files.SecretStoreCA, ca, 0600); err != nil {
		t.Fatal(err)
	}
	return backend
}
func (f *composedFixture) prepareSecretCommit(t *testing.T) {
	t.Helper()
	f.prepareCommit(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	document := strings.Replace(composedWorkflow, "  - name: build\n", "  - name: build\n    metadata:\n      annotations:\n        "+argoworkflow.SecretPathsAnnotation+": registry/key\n", 1)
	command, _ := json.Marshal([]string{"oberth", "secretstore", "exec", "--", binary, "-test.run=^TestComposedSecretChildProcess$", "--", "c11-composed-secret-child"})
	document = strings.Replace(document, `command: ["true"]`, "command: "+string(command), 1)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	f.git.Write(t, ".oberth/deploy.yaml", []byte(composedWorkflow))
	settings, err := decodeRuntime(f.config.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	settings.HelperImage = "oberthci/oberth@sha256:" + strings.Repeat("b", 64)
	settings.Routes[gittest.Repository] = append(settings.Routes[gittest.Repository], workflowpolicy.ReleaseRoute{Pattern: "ship-*", File: ".oberth/deploy.yaml"})
	f.config.Runtime, err = json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	f.git.CommitFile(t, "secret consumer")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
}
func (f *composedFixture) writeSecretThroughCLI(t *testing.T) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app := application{in: strings.NewReader("fixture-secret"), out: &out, diagnostic: &diagnostic, env: func(key string) string {
		if key == "OBERTH_URL" {
			return f.config.Public.URL
		}
		return ""
	}, config: client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport}}
	args := []string{"secret", "put", "registry/key", "--repo", gittest.Repository, "--file", ".oberth/build.yaml", "--step", "build"}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("private secret CLI write failed", code, diagnostic.String())
	}
	if strings.Contains(out.String()+diagnostic.String(), "fixture-secret") {
		t.Fatal("secret CLI exposed private bytes")
	}
}
func (f *composedFixture) checkLiveState(t *testing.T, remote *client.Client) {
	t.Helper()
	for _, name := range []string{"system_status", "admin_version"} {
		var status apicontract.SystemStatus
		if err := remote.Invoke(t.Context(), name, map[string]any{}, &status); err != nil || status.SchemaVersion != store.Schema || status.Protocol != f.config.Protocol {
			t.Fatal("administrator live metadata differs", status, err)
		}
	}
	options, err := liveInstallationState(t.Context(), remote)
	if err != nil || options.Schema != store.Schema || options.Protocol != f.config.Protocol {
		t.Fatal("upgrade floor did not use live state", options, err)
	}
}
func (f *composedFixture) consumeSecretStep(t *testing.T, r stateport.Run, source string) {
	t.Helper()
	template := r.Admission.Workflow.Spec.Templates[0]
	if template.Container == nil || template.ServiceAccountName == "" {
		t.Fatal("secret step has no native identity")
	}
	env := map[string]string{}
	for _, value := range template.Container.Env {
		env[value.Name] = value.Value
	}
	if env[runtimeconfig.SecretTokenEnv] != runtimeconfig.SecretTokenDirectory+"/token" || env[runtimeconfig.SecretScratchEnv] != runtimeconfig.SecretScratchDirectory {
		t.Fatal("native secret mount contract differs")
	}
	parent, err := os.MkdirTemp("/dev/shm", "c11-composed-secrets-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parent) })
	claims, _ := json.Marshal(map[string]string{"sub": "system:serviceaccount:" + f.config.PipelineNamespace + ":" + template.ServiceAccountName})
	token := filepath.Join(t.TempDir(), "projected-token")
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	if err = os.WriteFile(token, []byte(jwt), 0600); err != nil {
		t.Fatal(err)
	}
	env[runtimeconfig.SecretTokenEnv], env[runtimeconfig.SecretScratchEnv] = token, parent
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, f.binary, template.Container.Command[1:]...)
	command.Dir = source
	command.Env = []string{"HOME=", "XDG_CONFIG_HOME="}
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("bare secretstore exec failed", err, string(output))
	}
	checkDeliveryCleanup(t, parent, string(output))
}
func TestComposedSecretChildProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "c11-composed-secret-child" {
		return
	}
	dir := os.Getenv("OBERTH_SECRETS_DIR")
	secret, err := os.ReadFile(filepath.Join(dir, "registry", "key"))
	if err != nil || string(secret) != "fixture-secret" {
		t.Fatal("step did not receive written secret")
	}
	source, err := os.ReadFile("file.txt")
	if err != nil || string(source) != "exact source\n" {
		t.Fatal("step did not consume admitted source")
	}
	_, _ = os.Stdout.WriteString(dir)
	os.Exit(0)
}

func (f *composedFixture) checkReleaseSecretWrite(t *testing.T, remote *client.Client, backend *storefake.Store) {
	t.Helper()
	tuple := apicontract.AccessAllowArgs{Repo: gittest.Repository, Workflow: ".oberth/deploy.yaml", Step: "build", Secret: "registry/key"}
	var grant apicontract.Grant
	if err := remote.Invoke(t.Context(), "access_allow", tuple, &grant); err != nil {
		t.Fatal(err)
	}
	args := apicontract.SecretPutArgs{Repo: tuple.Repo, Workflow: tuple.Workflow, Step: tuple.Step, Secret: tuple.Secret}
	if _, err := remote.PutSecret(t.Context(), args, []byte("fixture-secret")); err != nil {
		t.Fatal(err)
	}
	account, err := identityname.Name(identityname.Key{Repository: tuple.Repo, File: tuple.Workflow, Step: tuple.Step}, identityname.Step)
	if err != nil {
		t.Fatal(err)
	}
	path := "/" + f.config.SecretStore.KVMount + "/steps/" + account + "/" + tuple.Secret
	if _, ok := backend.Resources[path]; !ok {
		t.Fatal("configured release routes were not passed to the server writer")
	}
}
