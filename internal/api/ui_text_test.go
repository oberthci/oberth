package api_test

import (
	"strings"
	"testing"
)

func TestUIStatusMarksUseBasicText(t *testing.T) {
	html, js, css := uiSources(t)
	// Middle dot separates facts. Status marks are CSS shapes, never font glyphs.
	// This checks owned copy; authored issue bodies and log text retain Unicode.
	for _, source := range []string{html, js, css} {
		for _, character := range source {
			if character > 127 && character != '·' {
				t.Fatalf("UI-owned text depends on an unlisted character: U+%04X", character)
			}
		}
	}
	if !strings.Contains(js, `class: "mark "`) || !strings.Contains(css, "border-radius: 50%") || !strings.Contains(css, "background: currentColor") {
		t.Fatal("CSS status marks missing")
	}
}
