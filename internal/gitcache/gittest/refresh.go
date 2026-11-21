package gittest

import (
	"os"
	"path/filepath"
	"testing"
)

// ExpireRefresh makes a fixture's next demand read contact its forge, as if
// the freshness interval had passed: both the refresh and the look markers go.
func ExpireRefresh(t *testing.T, store string) {
	t.Helper()
	for _, marker := range []string{"oberth-refreshed", "oberth-looked"} {
		if err := os.Remove(filepath.Join(store, marker)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}
