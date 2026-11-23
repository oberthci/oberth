package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestValidateBoundRepositoryExplainsOfflineEvidence(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonOutput], func(t *testing.T) {
			validateBoundRepository(t, jsonOutput)
		})
	}
}

func validateBoundRepository(t *testing.T, jsonOutput bool) {
	t.Helper()
	bindings := fake.Bindings()
	live := &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{
		OK: true, Checks: []apicontract.Check{{Name: "native identity", State: "verified"}},
	}}
	bindings.RunCheck.Handler = live
	app, out, diagnostic := fixture(t, bindings)
	app.directory = t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "oberth.repository", "source/team/widget"}, {"config", "oberth.uplinkProfile", "joined"}} {
		if _, err := runGit(t.Context(), app.directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	const path = ".oberth/build.yaml"
	if err := os.Mkdir(filepath.Join(app.directory, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.directory, path), []byte(starterWorkflow()), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"validate", path}
	if jsonOutput {
		args = append(args, "--json")
	}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	if _, _, calls := live.Observed(); calls != 0 {
		t.Fatal("offline validation made live checks", calls)
	}
	assertOfflineValidationOutput(t, out.String(), jsonOutput)
	out.Reset()
	if code := app.run(t.Context(), []string{"check", "--local", path, "--json"}); code != 0 || !strings.Contains(out.String(), `"state":"verified"`) {
		t.Fatal("bound live check lost verification", code, out.String(), diagnostic.String())
	}
}

func assertOfflineValidationOutput(t *testing.T, output string, jsonOutput bool) {
	t.Helper()
	if jsonOutput {
		var result validationResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || !result.OK || len(result.Needs) == 0 {
			t.Fatal(result, err)
		}
		for _, need := range result.Needs {
			if need.Code == "live" {
				assertOfflineLiveNeed(t, need.Reason, need.Remedy)
			}
		}
	} else if !strings.Contains(output, "valid offline") || !strings.Contains(output, "oberth check --repo '<repo>' --local '.oberth/build.yaml'") {
		t.Fatal("offline scope or live command missing", output)
	}
	if strings.Contains(output, "ask the operator") || strings.Contains(output, "before the first run") {
		t.Fatal("offline validation claimed missing authorization", output)
	}
}

func assertOfflineLiveNeed(t *testing.T, reason, remedy string) {
	t.Helper()
	if !strings.Contains(reason, "offline") || !strings.Contains(remedy, "oberth check") || publicerr.CheckMessage(reason, 40) != nil || publicerr.CheckMessage(remedy, 120) != nil {
		t.Fatal("misleading live requirement", reason, remedy)
	}
}
