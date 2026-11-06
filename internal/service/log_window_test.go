package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/runlog"
)

func TestRunLogsDefaultWindowAndExplicitFilters(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/window", "branch", true)
	var output strings.Builder
	for i := 1; i <= 350; i++ {
		fmt.Fprintf(&output, "[test/unit] line %03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(f.root, "logs", run.ID+".log"), []byte(output.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		fields        map[string]any
		lines         int
		first         string
		defaultWindow bool
	}{
		{"default", nil, 200, "151", true},
		{"context alone", map[string]any{"context": 2}, 200, "151", true},
		{"empty pattern", map[string]any{"pattern": ""}, 350, "001", false},
		{"case insensitive zero limit", map[string]any{"Limit": 0}, 350, "001", false},
		{"zero limit", map[string]any{"limit": 0}, 350, "001", false},
		{"false tail", map[string]any{"tail": false}, 350, "001", false},
		{"zero offset", map[string]any{"offset": 0}, 350, "001", false},
		{"zero byte offset", map[string]any{"byte_offset": 0}, 350, "001", false},
		{"larger tail", map[string]any{"tail": true, "limit": 300}, 300, "051", false},
		{"head page", map[string]any{"limit": 10, "offset": 3}, 10, "004", false},
		{"pattern", map[string]any{"pattern": "line 350"}, 1, "350", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"id": run.ID, "burn": "test", "step": "unit"}
			for k, v := range tc.fields {
				args[k] = v
			}
			raw, _ := json.Marshal(args)
			value, err := f.api.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", raw)
			if err != nil {
				t.Fatal(err)
			}
			got := value.(LogResponse)
			if got.ReturnedLines != tc.lines || !strings.HasPrefix(got.Output, "[test/unit] line "+tc.first+"\n") {
				t.Fatalf("lines=%d, output=%.80q", got.ReturnedLines, got.Output)
			}
			if tc.defaultWindow {
				if got.Window != runlog.DefaultWindowDescription || !strings.Contains(got.MCPToolText(), got.Window) {
					t.Fatal("missing default explanation")
				}
			} else if got.Window != "" {
				t.Fatal("explicit filter labeled default", got.Window)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), `"window":`) != tc.defaultWindow {
				t.Fatal("incorrect wire window")
			}
		})
	}
}
