package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnsealRequiresCoordinatesAndExplicitHostRisk(t *testing.T) {
	for _, args := range [][]string{{"unseal"}, {"unseal", "--secrets-file", "/private/file"}, {"unseal", "--context", "chosen", "--secrets-file", "/private/file", "--seal-to-tpm2", "/private/credential"}} {
		var output bytes.Buffer
		err := runCLI(t.Context(), args, strings.NewReader(""), &output)
		if err == nil || output.Len() != 0 {
			t.Fatal("expected refusal without output")
		}
	}
	var output bytes.Buffer
	if err := runCLI(t.Context(), []string{"unseal", "--help"}, strings.NewReader(""), &output); err != nil || !strings.Contains(output.String(), "-secrets-file") {
		t.Fatal("help must work offline")
	}
}
