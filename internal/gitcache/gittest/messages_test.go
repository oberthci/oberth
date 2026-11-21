package gittest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogueAccepts40Refuses41AndComputedText(t *testing.T) {
	for _, tc := range []struct {
		expression string
		valid      bool
	}{
		{`"` + strings.Repeat("a", 40) + `"`, true},
		{`"` + strings.Repeat("a", 41) + `"`, false},
		{`"short" + " phrase"`, false},
	} {
		file := filepath.Join(t.TempDir(), "messages.go")
		if err := os.WriteFile(file, []byte("package fixture\nconst reasonFixture="+tc.expression+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := MessageErrors(file); (len(got) == 0) != tc.valid {
			t.Fatalf("catalogue expression %s: %v", tc.expression, got)
		}
	}
}
