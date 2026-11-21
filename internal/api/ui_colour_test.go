package api_test

import (
	"math"
	"regexp"
	"strconv"
	"testing"
)

func luminance(hex string) float64 {
	n, _ := strconv.ParseUint(hex, 16, 32)
	value := 0.0
	for index, weight := range []float64{0.2126, 0.7152, 0.0722} {
		channel := float64((n>>uint(16-index*8))&255) / 255
		if channel <= 0.04045 {
			channel /= 12.92
		} else {
			channel = math.Pow((channel+0.055)/1.055, 2.4)
		}
		value += channel * weight
	}
	return value
}
// palettes reads each token's value per theme: light and dark from the paired
// tokens, then one value from each added theme's own block, which must set
// every token exactly once.
func palettes(t *testing.T, css string) (map[string][]string, int) {
	t.Helper()
	colours := map[string][]string{}
	for _, pair := range regexp.MustCompile(`--([a-z]+): light-dark\(#([0-9a-f]{6}), #([0-9a-f]{6})\)`).FindAllStringSubmatch(css, -1) {
		colours[pair[1]] = []string{pair[2], pair[3]}
	}
	blocks := regexp.MustCompile(`:root\[data-theme="[a-z]+"\] \{([^}]*)\}`).FindAllStringSubmatch(css, -1)
	for index, block := range blocks {
		for _, value := range regexp.MustCompile(`(?m)^ --([a-z]+): #([0-9a-f]{6});$`).FindAllStringSubmatch(block[1], -1) {
			colours[value[1]] = append(colours[value[1]], value[2])
		}
		for name, values := range colours {
			if len(values) != 3+index {
				t.Fatalf("%s has %d values", name, len(values))
			}
		}
	}
	return colours, 2 + len(blocks)
}
func TestUIColourTokens(t *testing.T) {
	_, _, css := uiSources(t)
	colours, found := palettes(t, css)
	const themes = 4 // light, dark, mono, color
	if len(colours) != 9 || found != themes || len(regexp.MustCompile(`#[0-9a-f]{6}`).FindAllString(css, -1)) != 9*themes {
		t.Fatal("colour outside paired tokens")
	}
	for _, name := range []string{"fg", "muted", "accent", "passed", "failed", "running"} {
		for _, background := range []string{"bg", "panel"} {
			for theme := range themes {
				a, b := luminance(colours[name][theme]), luminance(colours[background][theme])
				contrast := (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
				if contrast < 4.5 {
					t.Errorf("%s on %s theme %d: %.2f", name, background, theme, contrast)
				}
			}
		}
	}
}
