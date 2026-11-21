package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBuildDateComesFromTheSourceCommitInBothPasses(t *testing.T) {
	for _, date := range []string{"2026-11-17T12:34:56Z", "2026-11-18T15:24:00+01:00"} {
		t.Run(date, func(t *testing.T) {
			dir := releaseBuildTree(t)
			output, err := buildScript(t, dir, map[string]string{"COMMIT_DATE": date})
			if err != nil {
				t.Fatal(err, string(output))
			}
			assertReleaseBuildDates(t, dir, date)
		})
	}
}

func assertReleaseBuildDates(t *testing.T, dir, date string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasSuffix(line, "./cmd/oberth") {
			continue
		}
		count++
		if !strings.Contains(line, "-X main.date="+date) {
			t.Fatal("CLI date differs from source commit", line)
		}
	}
	if count != 8 {
		t.Fatal("both reproducibility passes must date all four CLI targets", count)
	}
}
