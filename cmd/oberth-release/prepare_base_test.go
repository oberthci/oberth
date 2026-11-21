package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestWorkflowsPrepareVerifiedBase(t *testing.T) {
	for _, name := range []string{"build", "release"} {
		for _, state := range []string{"valid", "missing-remote", "missing-index", "wrong-digest"} {
			t.Run(name+"/"+state, func(t *testing.T) { checkWorkflowBase(t, name, state) })
		}
	}
}

func checkWorkflowBase(t *testing.T, name, state string) {
	t.Helper()
	wf, err := argoworkflow.Decode(ownWorkflow(t, name))
	if err != nil {
		t.Fatal(err)
	}
	prepare := wf.GetTemplateByName("prepare-base")
	if prepare == nil {
		t.Fatal("workflow has no base preparation")
	}
	assertBaseDependency(t, name)
	dir := workflowFixture(t)
	configureBaseFixture(t, dir)
	breakBaseFixture(t, dir, state)
	values := workflowValues(dir)
	values["BASE_FETCH_FAILURE"] = state
	output, err := executeWorkflowTemplate(t, wf, prepare, values)
	assertBasePreparationResult(t, state, output, err)
	if state == "missing-remote" {
		assertBaseFetchContext(t, dir, output)
	}
	base := filepath.Join(dir, "shared/base-image")
	if state == "valid" {
		assertPreparedBase(t, dir, base)
	} else if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("failed preparation exposed a base", err)
	}
	stages, err := filepath.Glob(filepath.Join(dir, "shared/.base-image.*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("base staging files retained", stages, err)
	}
}

func assertBasePreparationResult(t *testing.T, state string, output []byte, err error) {
	t.Helper()
	if (err == nil) != (state == "valid") {
		t.Fatal("base preparation result differs", err, string(output))
	}
	reasons := map[string]string{"missing-remote": "Release base fetch failed; publish the pin or check registry access.", "missing-index": "Release base index is missing;", "wrong-digest": string(baseMismatch)}
	if state != "valid" && !strings.Contains(string(output), reasons[state]) {
		t.Fatal("base preparation lost the refusal", state, string(output))
	}
}

func assertBaseDependency(t *testing.T, name string) {
	t.Helper()
	wf, err := argoworkflow.Decode(ownWorkflow(t, name))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range wf.GetTemplateByName(name).DAG.Tasks {
		if task.Name == "release-bytes-build" {
			found = task.Depends == "prepare-base"
		}
	}
	if !found {
		t.Fatal("byte build does not depend on verified base preparation")
	}
}

func configureBaseFixture(t *testing.T, dir string) {
	t.Helper()
	for _, file := range []string{"hack/prepare-base.sh", "hack/base-image/check.sh", "hack/tools/crane.mod", ".oberth/pins/base-image.repository"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0755); err != nil {
			t.Fatal(err)
		}
		copyFile(t, "../../"+file, filepath.Join(dir, file))
	}
	o := options{base: filepath.Join(dir, "base-source"), pin: filepath.Join(dir, ".oberth/pins/base-image.digest")}
	setupBase(t, o)
	index, err := layout.ImageIndexFromPath(o.base)
	if err != nil {
		t.Fatal(err)
	}
	pulled, err := layout.Write(filepath.Join(dir, "registry-fixture"), empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := pulled.AppendIndex(index); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bin/go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "#!/bin/sh\n", "#!/bin/sh\n"+baseFixtureCommands(), 1))
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
}

func baseFixtureCommands() string {
	return `if [ "$1" = tool ] && [ "${3:-}" = crane ]; then
  [ "$#" = 7 ] && [ "$2" = -modfile=hack/tools/crane.mod ] && [ "$4" = pull ] && [ "$5" = --format=oci ] || exit 94
  [ "$6" = "$(cat .oberth/pins/base-image.repository)@$(cat .oberth/pins/base-image.digest)" ] || exit 95
  printf '%s\n' "$*" > "$FIXTURE_ROOT/fetch.log"
  if [ "${BASE_FETCH_FAILURE:-}" = missing-remote ]; then echo 'fixture registry: manifest unknown' >&2; exit 96; fi
  cp -R "$FIXTURE_ROOT/registry-fixture/." "$7/"
  exit
fi
if [ "$1" = build ] && [ "${2:-}" = -buildvcs=false ] && [ "${3:-}" = -o ]; then
  cat > "$4" <<'CHECK'
#!/bin/sh
[ "$1" = base ] && [ "$2" = --base ] && [ "$4" = --pin ] || exit 97
exec env GORACE=atexit_sleep_ms=0 BASE_CHECK_HELPER=1 BASE_CHECK_LAYOUT="$3" BASE_CHECK_PIN="$5" "$BASE_TEST_BINARY" -test.run=^TestPrepareBaseChecker$ -test.timeout=30s -test.count=1
CHECK
  chmod 700 "$4"
  exit
fi
`
}

func breakBaseFixture(t *testing.T, dir, state string) {
	t.Helper()
	pin, err := os.ReadFile(filepath.Join(dir, ".oberth/pins/base-image.digest"))
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "registry-fixture/blobs/sha256", strings.TrimPrefix(strings.TrimSpace(string(pin)), "sha256:"))
	switch state {
	case "missing-index":
		err = os.Remove(blob)
	case "wrong-digest":
		err = os.WriteFile(blob, []byte("{}"), 0644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertPreparedBase(t *testing.T, dir, base string) {
	t.Helper()
	pin := filepath.Join(dir, ".oberth/pins/base-image.digest")
	if _, err := checkedBase(options{base: base, pin: pin, git: fixturePin}); err != nil {
		t.Fatal("prepared root differs from the exact two-platform index", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fetch.log")); err != nil {
		t.Fatal("base was not fetched", err)
	}
}

func TestPrepareBaseChecker(t *testing.T) {
	if os.Getenv("BASE_CHECK_HELPER") != "1" {
		return
	}
	err := checkBase(options{base: os.Getenv("BASE_CHECK_LAYOUT"), pin: os.Getenv("BASE_CHECK_PIN"), git: fixturePin})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareBaseRefusesInvalidInputs(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pin", "output", "target"} {
		t.Run(state, func(t *testing.T) {
			dir := workflowFixture(t)
			configureBaseFixture(t, dir)
			values := workflowValues(dir)
			base := filepath.Join(dir, "shared/base-image")
			reason := invalidBaseInput(t, dir, state, values)
			output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("prepare-base"), values)
			if err == nil || !strings.Contains(string(output), reason) {
				t.Fatal("invalid preparation input did not refuse", err, string(output))
			}
			if _, err := os.Stat(filepath.Join(dir, "fetch.log")); !os.IsNotExist(err) {
				t.Fatal("invalid input reached the registry", err)
			}
			if state == "output" {
				body, err := os.ReadFile(base)
				if err != nil || string(body) != "retained" {
					t.Fatal("existing output was changed", err, string(body))
				}
			}
		})
	}
}

func invalidBaseInput(t *testing.T, dir, state string, values map[string]string) string {
	t.Helper()
	var reason string
	var err error
	switch state {
	case "pin":
		reason = "Release base pin is invalid;"
		err = os.WriteFile(filepath.Join(dir, ".oberth/pins/base-image.digest"), []byte("invalid"), 0644)
	case "output":
		reason = "Release base output exists;"
		err = os.WriteFile(filepath.Join(dir, "shared/base-image"), []byte("retained"), 0644)
	case "target":
		reason = "Set RELEASE_BASE to this run's work volume"
		values["RELEASE_BASE"] = ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func assertBaseFetchContext(t *testing.T, dir string, output []byte) {
	t.Helper()
	for _, item := range []struct{ label, file string }{
		{"Repository", "base-image.repository"}, {"Digest", "base-image.digest"},
	} {
		body, err := os.ReadFile(filepath.Join(dir, ".oberth/pins", item.file))
		if err != nil || !strings.Contains(string(output), item.label+": "+strings.TrimSpace(string(body))) {
			t.Fatal("fetch refusal lost the requested input", item.label, err, string(output))
		}
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Release base fetch failed;") && len(line) > 80 {
			t.Fatal("fetch refusal exceeds 80 characters", line)
		}
	}
}
