package main

import (
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/api/fake"
	"strings"
	"testing"
)

func TestScheduledRunIdentifiersRemainWholeAndDistinct(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	rows := []map[string]string{}
	for i := range 5 {
		rows = append(rows, map[string]string{"id": fmt.Sprintf("oberth-cron-%s-%d", strings.Repeat("a", 80), i), "status": "passed", "ref": "main", "display_reason": "passed"})
	}
	data, _ := json.Marshal(map[string]any{"runs": rows, "next_cursor": rows[4]["id"]})
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	for i, row := range rows {
		if fields := strings.Fields(lines[i+1]); len(fields) != 4 || fields[0] != row["id"] {
			t.Fatal("identifier was shortened or wrapped", out.String())
		}
	}
	if !strings.Contains(out.String(), "\noberth run list --before "+rows[4]["id"]+"\n") {
		t.Fatal("cursor command was split", out.String())
	}
}
