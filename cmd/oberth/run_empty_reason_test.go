package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDisplaysReadWellWithoutAReason(t *testing.T) {
	for _, status := range []string{"queued", "running", "passed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			run := map[string]any{"id": "12345678", "status": status, "display_reason": "", "repo": "origin/team/web", "ref": "refs/heads/main"}
			var detail, list bytes.Buffer
			if err := renderRun(&detail, run, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(detail.String(), "Run 12345678  "+status) || strings.Contains(detail.String(), "Display reason") {
				t.Fatal("empty reason added a fact or hid status", detail.String())
			}
			if err := renderRuns(&list, []any{run}, nil, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(list.String(), status) || !strings.Contains(list.String(), "main") {
				t.Fatal("empty reason hid run list facts", list.String())
			}
		})
	}
}
