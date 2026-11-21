package gitoid

import (
	"strings"
	"testing"
)

func TestCanonicalObjectIdentifiers(t *testing.T) {
	for _, size := range []int{40, 64} {
		for _, digit := range "0123456789abcdef" {
			if !Valid(strings.Repeat(string(digit), size)) {
				t.Fatalf("canonical %d-byte spelling refused", size)
			}
		}
	}
}
func TestNoncanonicalIdentifiersRefuse(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 40), " " + strings.Repeat("a", 40), strings.Repeat("a", 39) + "g", strings.Repeat("a", 39) + "\n"} {
		t.Run(value, func(t *testing.T) {
			if Valid(value) {
				t.Fatal("noncanonical identifier accepted")
			}
		})
	}
}
