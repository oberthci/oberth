package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestSigningDownloadDocumentationMatchesThePinnedExecutable(t *testing.T) {
	script, err := os.ReadFile("../../hack/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{`https://github\.com/[^\s]+`, `[0-9a-f]{64}`} {
		value := regexp.MustCompile(pattern).FindString(string(script))
		if value == "" || !strings.Contains(string(doc), value) {
			t.Fatal("signing-tool download documentation differs from the executable pin", value)
		}
	}
}
