package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBackupJSONWarningFieldsAreDocumented(t *testing.T) {
	data, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "## Back up")
	if !found {
		t.Fatal("backup documentation is missing")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	warning := publicerr.New(publicerr.InvalidInput, "Backup is published.", "Keep the backup.", nil)
	warning.ID, warning.Context, warning.Detail = "publication", []string{"backup.age"}, "storage diagnostic"
	encoded, err := json.Marshal(warning)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		if !strings.Contains(section, "`"+field+"`") {
			t.Error("public warning field is undocumented", field)
		}
	}
	for _, fact := range []string{"--output json", "exit 0", "optional `warning`", "standard error", "clean and dry-run results omit `warning`"} {
		if !strings.Contains(section, fact) {
			t.Error("backup warning fact is missing", fact)
		}
	}
}
