package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPSPreflightListsNamesAndRefusesBeforeUpgrade(t *testing.T) {
	metadata := "NAME KIND URL KEY NAME KEY FINGERPRINT\ngithub https https://github.com/fixture - -\ncodeberg ssh ssh://git@codeberg.org/fixture - -\n"
	path := filepath.Join(t.TempDir(), "upstreams.txt")
	if err := os.WriteFile(path, []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runPreflightUpstreams([]string{"--inventory", path}, &output)
	if err == nil || !strings.Contains(err.Error(), "upgrade refused") || output.String() != "HTTPS upstream: github\n" {
		t.Fatalf("preflight: %q %v", output.String(), err)
	}
	for _, input := range []string{"", "partial\n", "NAME KIND URL\nbad\n", "NAME KIND URL\na ssh ssh://git@host/org\na ssh ssh://git@host/org\n"} {
		if _, err := httpsUpstreamInventory(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted incomplete inventory %q", input)
		}
	}
	names, err := httpsUpstreamInventory(strings.NewReader("NAME KIND URL\na ssh ssh://git@host/org\nb local /local/forge\n"))
	if err != nil || len(names) != 0 {
		t.Fatalf("safe inventory: %v %v", names, err)
	}
}
func TestHTTPSUpstreamRegistrationRefusesBeforeAnySideEffect(t *testing.T) {
	var output bytes.Buffer
	err := runUpstreamWithDependencies(t.Context(), []string{"add", "name", "https://forge.example/acme"}, &output, upstreamDependencies{})
	if err == nil || !strings.Contains(err.Error(), "built without curl") {
		t.Fatalf("registration advanced to mutation/database/key access: %v", err)
	}
}
