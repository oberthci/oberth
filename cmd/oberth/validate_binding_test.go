package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestValidateIgnoresMalformedLocalRepositoryBinding(t *testing.T) {
	for _, binding := range []string{"widget", "source/team/widget.git", "/source/team/widget", "global/team/widget"} {
		for _, format := range []string{"text", "json"} {
			t.Run(binding+"/"+format, func(t *testing.T) {
				validateMalformedBinding(t, binding, format == "json")
			})
		}
	}
}

func validateMalformedBinding(t *testing.T, binding string, jsonOutput bool) {
	t.Helper()
	bindings := fake.Bindings()
	live := &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{}
	bindings.RunCheck.Handler = live
	app, out, diagnostic := fixture(t, bindings)
	app.directory = t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "oberth.repository", binding}} {
		if _, err := runGit(t.Context(), app.directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	const path = ".oberth/build.yaml"
	if err := os.Mkdir(filepath.Join(app.directory, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.directory, path), validationGrantDocument(t), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"validate", path}
	if jsonOutput {
		args = append(args, "--json")
	}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("malformed binding changed offline validation", code, out.String(), diagnostic.String())
	}
	if _, _, calls := live.Observed(); calls != 0 {
		t.Fatal("malformed binding triggered live checks", calls)
	}
	assertMalformedBindingOutput(t, out.String(), jsonOutput)
}

func assertMalformedBindingOutput(t *testing.T, output string, jsonOutput bool) {
	t.Helper()
	if jsonOutput {
		var result validationResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || !result.OK || len(result.Refusals) != 0 {
			t.Fatal("invalid offline result", result, err)
		}
		output = ""
		for _, need := range result.Needs {
			output += need.Reason + " " + need.Remedy + " " + need.Command + "\n"
		}
	}
	if !strings.Contains(output, "credential grant is unverified") || !strings.Contains(output, "<repo>") || strings.Contains(output, "Ask the operator") {
		t.Fatal("invalid binding became authorization evidence", output)
	}
}
