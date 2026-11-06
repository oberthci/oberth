package main

import (
	"os"
	"strings"
	"testing"
)

func TestLegacyExecutionDeprecationIsDocumented(t *testing.T) {
	body, err := os.ReadFile("../../docs/workflow-capabilities.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"no longer", "OBERTH_KVM", "OBERTH_GUEST_TEST", ".oberth/tests.yaml", "KubeVirt"} {
		if !strings.Contains(string(body), term) {
			t.Errorf("compatibility contract missing %s", term)
		}
	}
}
