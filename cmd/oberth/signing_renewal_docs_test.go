package main

import (
	"os"
	"strings"
	"testing"
)

func TestSigningRenewalGuidesRequireTheNewClient(t *testing.T) {
	for _, name := range []string{"releasing.md", "operations.md"} {
		raw, err := os.ReadFile("../../docs/" + name)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, instruction := range []string{"After replacing the signing key, run upgrades with", "the new release's client; an older client pins the earlier key and refuses the new release."} {
			if !strings.Contains(text, instruction) {
				t.Fatal(name, "omits signing renewal guidance", instruction)
			}
		}
	}
}
