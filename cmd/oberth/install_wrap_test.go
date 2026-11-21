package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// Install output wraps between words only; a path gets a line of its own and
// the sentence around it stays whole on either side.
func TestInstallOutputKeepsPathsAndSentencesWhole(t *testing.T) {
	key := "/home/operator/.config/oberth/" + strings.Repeat("profile-", 6) + "second/backup.agekey"
	recovery := "/home/operator/.config/oberth/" + strings.Repeat("profile-", 6) + "second/recovery"
	result := installer.Result{BackupReminder: "Copy " + key + " off this machine.\nCopy " + recovery + " off this machine."}
	var out bytes.Buffer
	if err := showInstallation(&out, result); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	for _, path := range []string{key, recovery} {
		if !containsLine(lines, path) {
			t.Fatal("path broken or joined to prose", path, out.String())
		}
	}
	if strings.Count(out.String(), "off this machine.") != 2 || strings.Contains(out.String(), "off this\n") {
		t.Fatal("sentence broken around the path", out.String())
	}
	for _, line := range lines {
		if len([]rune(line)) > 80 && strings.TrimSpace(line) != key && strings.TrimSpace(line) != recovery {
			t.Fatal("prose line over 80 columns", line)
		}
	}
}

func TestWrappingNeverSplitsAWord(t *testing.T) {
	long := strings.Repeat("x", 100)
	var out bytes.Buffer
	if err := writeWrapped(&out, "Short words before "+long+" and short words after it that keep going past the edge."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), long) {
		t.Fatal("an overlong word was split", out.String())
	}
}

func containsLine(lines []string, value string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == value {
			return true
		}
	}
	return false
}
