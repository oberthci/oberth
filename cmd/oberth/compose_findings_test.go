//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestValidateDefaultAndJSONMatchFiveActualPushRefusals(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	for _, scenario := range []struct{ name, before, after string }{
		{"patch", "spec:\n", "spec:\n  podSpecPatch: '{}'\n  hostNetwork: true\n"},
		{"network", "spec:\n", "spec:\n  hostNetwork: true\n"},
		{"label", "kind: Workflow\n", "kind: Workflow\nmetadata:\n  labels: {oberth.ci/managed: injected}\n"},
		{"parameter", "spec:\n", "spec:\n  arguments:\n    parameters: [{name: oberth-credential, value: injected}]\n"},
		{"http", "    container:\n", "    http: {url: 'https://oberth.ci'}\n    container:\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			document := strings.Replace(composedWorkflow, scenario.before, scenario.after, 1)
			f.git.Write(t, ".oberth/build.yaml", []byte(document))
			f.git.CommitFile(t, "refused "+scenario.name)
			pushed, err := f.pushResult(t, "refs/heads/"+scenario.name, "-o", "oberth.run=1")
			if err == nil {
				t.Fatal("push accepted invalid workflow", pushed)
			}
			if scenario.name == "patch" && strings.Count(pushed, "\n  fix: ") != 2 {
				t.Fatal("push did not list both independent findings", pushed)
			}
			compareValidationToPush(t, f.git.Work, pushed)
		})
	}
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	if err != nil || len(runs) != 0 {
		t.Fatal("refused validation shapes created runs", runs, err)
	}
}

func compareValidationToPush(t *testing.T, root, pushed string) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = root
	if code := app.run(t.Context(), []string{"validate", ".oberth/build.yaml"}); code != 1 {
		t.Fatal("human validation did not refuse", code, diagnostic.String())
	}
	human := diagnostic.String()
	out.Reset()
	diagnostic.Reset()
	if code := app.run(t.Context(), []string{"validate", ".oberth/build.yaml", "-o", "json"}); code != 1 {
		t.Fatal("JSON validation did not refuse", code, diagnostic.String())
	}
	var result validationResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Refusals) == 0 || len(result.Refusals) != strings.Count(pushed, "\n  fix: ") {
		t.Fatal("refusal findings list missing", out.String(), err)
	}
	for _, row := range result.Refusals {
		if result.File != ".oberth/build.yaml" {
			t.Fatal("finding lost file", row)
		}
		for _, text := range []string{result.File, row.Path, row.Reason, row.Remedy} {
			if !strings.Contains(strings.Join(strings.Fields(human), " "), text) || !strings.Contains(pushed, text) {
				t.Fatalf("validate and push disagree on %q\nhuman: %s\npush: %s", text, human, pushed)
			}
		}
	}
	assertFindingOutputWidth(t, human)
}
