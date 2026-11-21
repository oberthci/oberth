package gitcache_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestPendingReconciliationMarkerAccepts4MiBRefusesOneMoreByte(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	for _, size := range []int{4 << 20, (4 << 20) + 1} {
		name := "refs/heads/main\n"
		path := filepath.Join(f.Store(), "oberth-reconcile")
		if err := os.WriteFile(path, []byte(name+strings.Repeat(" ", size-len(name))), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main")
		if err != nil {
			t.Fatal("marker prevented serving", err)
		}
		_, aside := os.Stat(path + ".stuck")
		if (aside == nil) != (size > 4<<20) {
			t.Fatal("marker bound", size, aside)
		}
	}
}
