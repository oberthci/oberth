package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateUnverifiedGrantPrintsCheckCommand(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonOutput], func(t *testing.T) {
			output := validateBoundRepository(t, jsonOutput, validationGrantDocument(t))
			assertUnverifiedGrantCommand(t, output, jsonOutput)
		})
	}
}

func assertUnverifiedGrantCommand(t *testing.T, output string, jsonOutput bool) {
	t.Helper()
	if !strings.Contains(output, "credential grant is unverified") || strings.Contains(output, "oberth secret put") {
		t.Fatal("unchecked grant suggests a write", output)
	}
	if !jsonOutput {
		return
	}
	var result validationResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	for _, need := range result.Needs {
		if need.Code == "grant" && need.Command != "oberth check --repo 'source/team/widget' --local '.oberth/build.yaml'" {
			t.Fatal("unchecked grant command differs from remedy", need)
		}
	}
}
