package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestInstallRejectsClusterCreationOption(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var output bytes.Buffer
	err := runCLI(context.Background(), []string{"install", "--create-kind"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("cluster creation option was accepted: %v", err)
	}
}
