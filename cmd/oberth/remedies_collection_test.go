package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemedyCollectionIncludesNewCatalogues(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd/new-command", "internal/new-owner", "pkg/new-adapter"} {
		path := filepath.Join(root, directory)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"messages.go":       "package example\nconst message = \"Run oberth \" + \"restore.\"\n",
			"messages_linux.go": "//go:build linux\n\npackage example\nconst other = \"Use missing-tool --repair.\"\n",
			"metadata.go":       "package example\ntype Metadata struct { Field string `json:\"cursor\" desc:\"Public description.\" summary:\"Public summary.\"` }\n",
			"messages_test.go":  "package example\nconst control = \"Ignore this test fixture.\"\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows := remedyGoTexts(t, root)
	if len(rows) != 15 {
		t.Fatalf("new owners and platform files must be collected; tests must be excluded: %+v", rows)
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Text]++
	}
	if counts["Run oberth restore."] != 3 || counts["Use missing-tool --repair."] != 3 || counts["Public description."] != 3 || counts["Public summary."] != 3 || counts["cursor"] != 3 {
		t.Fatal(counts)
	}
}
