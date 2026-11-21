package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRawToolHelpUsesItsJSONInvocation(t *testing.T) {
	for _, name := range []string{"issue_create", "issue_import"} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			cli := application{in: strings.NewReader(""), out: &out, diagnostic: &diagnostic}
			if code := cli.run(t.Context(), []string{"help", "tool", name}); code != 0 {
				t.Fatal(code, diagnostic.String())
			}
			want := "oberth tool " + name + " [JSON|-] [flags]\n"
			if !strings.HasPrefix(out.String(), want) {
				t.Fatal("raw JSON help must not require separate field arguments", out.String())
			}
			if strings.Contains(out.String(), "Use oberth  -") {
				t.Fatal("stdin guidance omitted its tool name", out.String())
			}
			if name == "issue_import" && !strings.Contains(out.String(), "Use oberth tool issue_import -") {
				t.Fatal("private import guidance must name its real raw command", out.String())
			}
		})
	}
}
