package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialedRepo is a checkout that needs a token to install its
// dependencies, which is the only case where the org matters: it is what
// scopes the secret path the pipeline declares.
func credentialedRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"),
		`{"name":"app","scripts":{"build":"tsc"},"devDependencies":{"typescript":"5"}}`+"\n")
	writeTestFile(t, filepath.Join(dir, ".npmrc"),
		"//npm.pkg.github.com/:_authToken=${NPM_TOKEN}\n")
	if origin != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, ".git", "config"),
			"[remote \"origin\"]\n\turl = "+origin+"\n")
	}
	return dir
}

func generatedPipeline(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// The defect: a checkout whose origin is a local path yielded "Documents" as
// the org, and the secret path built from it was refused at admission. There
// is no guess left to fall back to, so this has to fail and say what to do.
func TestInitWillNotGuessTheOrgWhenTheServerIsUnreachable(t *testing.T) {
	dir := credentialedRepo(t, "/Users/me/Documents/app")
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_TOKEN", "")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")

	var output bytes.Buffer
	err := executeInit(context.Background(), dir, "", "", false, &output)
	if err == nil {
		t.Fatal("init generated a pipeline without knowing the org")
	}
	message := err.Error()
	for _, want := range []string{"OBERTH_BASE_URL", "--org"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not name the remedy %q: %s", want, message)
		}
	}
	if strings.Contains(message, "Documents") {
		t.Errorf("the origin path leaked into the failure: %s", message)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".oberth", "build.yaml")); statErr == nil {
		t.Error("a pipeline was written despite the failure")
	}
}

// --org is the answer for a machine that cannot reach the server, and it wins
// over anything the server would have said.
func TestInitTakesTheOrgFromTheFlag(t *testing.T) {
	dir := credentialedRepo(t, "https://github.com/wrongorg/app.git")
	configure(t, remoteServer(t, `{"upstream_info":[{"name":"forge","kind":"https","base_url":"https://github.com/serverorg"}]}`))

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "flagorg", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generatedPipeline(t, dir), "oberth/upstream/flagorg/github-token") {
		t.Fatalf("the flag's org was not used:\n%s", generatedPipeline(t, dir))
	}
}

// The registered upstream is what admission matches against, so it is what the
// generated path is built from -- not the remote this checkout happens to have.
func TestInitTakesTheOrgFromTheServersRegistration(t *testing.T) {
	dir := credentialedRepo(t, "/Users/me/Documents/app")
	configure(t, remoteServer(t, `{"upstream_info":[{"name":"forge","kind":"https","base_url":"https://github.com/serverorg"}]}`))

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
		t.Fatal(err)
	}
	document := generatedPipeline(t, dir)
	if !strings.Contains(document, "oberth/upstream/serverorg/github-token") {
		t.Fatalf("the registered org was not used:\n%s", document)
	}
	if strings.Contains(document, "oberth/upstream/Documents") {
		t.Fatalf("the origin path reached the secret path:\n%s", document)
	}
	if !strings.Contains(output.String(), "serverorg") {
		t.Fatalf("the summary does not say where the org came from:\n%s", output.String())
	}
}

// A disagreement is information. The registered org is used, and the file says
// the other one exists rather than leaving the reader to wonder.
func TestInitSaysWhenTheOriginDisagreesWithTheRegistration(t *testing.T) {
	dir := credentialedRepo(t, "https://github.com/otherorg/app.git")
	configure(t, remoteServer(t, `{"upstream_info":[{"name":"forge","kind":"https","base_url":"https://github.com/serverorg"}]}`))

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
		t.Fatal(err)
	}
	document := generatedPipeline(t, dir)
	if !strings.Contains(document, "otherorg") || !strings.Contains(document, "serverorg") {
		t.Fatalf("the header does not mention the disagreement:\n%s", document)
	}
	if !strings.Contains(document, "oberth/upstream/serverorg/github-token") {
		t.Fatalf("the origin's org won:\n%s", document)
	}
}

// Two upstreams cannot both be "the" org, and picking the first would be the
// same guess in a different place.
func TestInitAsksWhichOrgWhenTheServerHasSeveral(t *testing.T) {
	dir := credentialedRepo(t, "")
	configure(t, remoteServer(t, `{"upstream_info":[`+
		`{"name":"a","base_url":"https://github.com/first"},`+
		`{"name":"b","base_url":"https://gitlab.com/second"}]}`))

	var output bytes.Buffer
	err := executeInit(context.Background(), dir, "", "", false, &output)
	if err == nil {
		t.Fatal("init picked one of two orgs on its own")
	}
	if !strings.Contains(err.Error(), "--org") {
		t.Errorf("the message does not name the remedy: %v", err)
	}
}

// A repository that pulls nothing private needs no org, and must still be
// initializable on a machine that cannot reach the server.
func TestInitNeedsNoOrgWhenThePipelineNeedsNoSecret(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\ngo 1.22\n")
	t.Setenv("OBERTH_BASE_URL", "")

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
		t.Fatalf("an offline init of a repository with no secret failed: %v", err)
	}
}

// A server that answers but has registered nothing has no org to give, and
// saying so is better than a path that will be refused.
func TestInitReportsAnUnregisteredServer(t *testing.T) {
	dir := credentialedRepo(t, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"upstream_info":[]}`))
	}))
	t.Cleanup(server.Close)
	configure(t, server)

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err == nil {
		t.Fatal("init accepted a server with no registered upstream")
	}
}
