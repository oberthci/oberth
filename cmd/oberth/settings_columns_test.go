package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestSettingsSourcePrecedesLongValue(t *testing.T) {
	image := "registry.example/" + strings.Repeat("digest", 24)
	var out bytes.Buffer
	if err := renderSettings(&out, map[string]any{"settings": []any{map[string]any{"name": "image", "source": "chosen", "value": image}}}, false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "image" {
			if !slices.Equal(fields, []string{"image", "chosen", image}) {
				t.Fatal("marker follows the long value", line)
			}
			return
		}
	}
	t.Fatal("settings row missing", out.String())
}
