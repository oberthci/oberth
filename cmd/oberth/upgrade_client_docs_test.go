package main

import (
	"os"
	"strings"
	"testing"
)

func TestUpgradeGuideReplacesTheAdministratorClientAndWrappers(t *testing.T) {
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(text, "After upgrading, replace the administrator CLI with that release's verified client and repoint wrappers to it.") {
		t.Fatal("upgrade guide leaves wrappers on the earlier client")
	}
}
