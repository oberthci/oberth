package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestGettingStartedFollowsReadmeSequence(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"help"}) != 0 {
		t.Fatal(diagnostic.String())
	}
	_, group, _ := strings.Cut(out.String(), "Getting started:\n")
	group, _, _ = strings.Cut(group, "Daily use:\n")
	for _, name := range []string{"init", "repo add"} {
		if commandGroup(name) != "Getting started" || !strings.Contains(group, name+": ") {
			t.Fatal("first-day command missing", name, group)
		}
	}
	if strings.Index(group, "init: ") > strings.Index(group, "install: ") {
		t.Fatal("starter comes after installation")
	}
	seen, previous := map[string]bool{}, -1
	for _, command := range readmeFirstDayCommands(t) {
		for _, name := range gettingStartedCommands() {
			if seen[name] || !strings.HasPrefix(command+" ", "oberth "+name+" ") {
				continue
			}
			position := strings.Index(group, name+": ")
			if position <= previous {
				t.Fatal("getting-started order differs from README", name, group)
			}
			seen[name], previous = true, position
		}
	}
}

func readmeFirstDayCommands(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "## First green build\n")
	if !ok {
		t.Fatal("README first-build section missing")
	}
	body, _, _ = strings.Cut(body, "## First release\n")
	var commands []string
	pending := ""
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced && line != "" {
			pending += strings.TrimSuffix(line, "\\")
			if strings.HasSuffix(line, "\\") {
				continue
			}
			commands = append(commands, strings.Join(strings.Fields(pending), " "))
			pending = ""
		}
	}
	return commands
}
