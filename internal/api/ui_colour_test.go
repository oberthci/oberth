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
func TestUIColourTokens(t *testing.T) {
	_, _, css := uiSources(t)
	pairs := regexp.MustCompile(`--([a-z]+): light-dark\(#([0-9a-f]{6}), #([0-9a-f]{6})\)`).FindAllStringSubmatch(css, -1)
	colours := map[string][2]string{}
	for _, pair := range pairs {
		colours[pair[1]] = [2]string{pair[2], pair[3]}
	}
	if len(colours) != 9 || len(regexp.MustCompile(`#[0-9a-f]{6}`).FindAllString(css, -1)) != 18 {
		t.Fatal("colour outside paired tokens")
	}
	for _, name := range []string{"fg", "muted", "accent", "passed", "failed", "running"} {
		for _, background := range []string{"bg", "panel"} {
			for theme := range 2 {
				a, b := luminance(colours[name][theme]), luminance(colours[background][theme])
				contrast := (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
				if contrast < 4.5 {
					t.Errorf("%s on %s theme %d: %.2f", name, background, theme, contrast)
				}
			}
		}
	}
}
