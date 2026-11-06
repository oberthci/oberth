package argojob

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDockerfileStartsWithPinnedFrontend(t *testing.T) {
	body, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(body), "\n")
	if !regexp.MustCompile(`^# syntax=docker/dockerfile:[^ ]+@sha256:[a-f0-9]{64}$`).MatchString(first) {
		t.Fatalf("pinned BuildKit parser directive must be the first line: %q", first)
	}
}
