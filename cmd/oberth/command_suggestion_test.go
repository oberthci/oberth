package main

import (
	"strings"
	"testing"
)

func TestRemoteHelpHasOneUsageLine(t *testing.T) {
	for _, command := range []string{"run", "runs", "log", "repos", "status", "issues", "issues update"} {
		t.Run(command, func(t *testing.T) {
			var out strings.Builder
			args := append(strings.Fields(command), "--help")
			if err := runCLI(t.Context(), args, nil, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.String(), "Usage:") != 1 || !strings.Contains(out.String(), "Usage: oberth "+command) {
				t.Fatal(out.String())
			}
		})
	}
}
func TestUnknownCommandSuggestsNearestName(t *testing.T) {
	for input, want := range map[string]string{"logs": "log", "stauts": "status", "upstrean": "upstream", "unrelated-word": ""} {
		var output strings.Builder
		err := runCLI(t.Context(), []string{input}, nil, &output)
		if err == nil {
			t.Fatal(input)
		}
		if want != "" && !strings.Contains(err.Error(), `did you mean "`+want+`"`) {
			t.Fatal(err)
		}
		if want == "" && strings.Contains(err.Error(), "did you mean") {
			t.Fatal(err)
		}
	}
}
