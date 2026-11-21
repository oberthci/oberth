package gitcache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestListRefsAccepts4096Refuses4097WithoutTruncation(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	var packed strings.Builder
	packed.WriteString("# pack-refs with: sorted\n")
	for i := 0; i < sourcecontract.MaxRefs-1; i++ {
		fmt.Fprintf(&packed, "%s refs/heads/b%04d\n", f.Commit, i)
	}
	fmt.Fprintf(&packed, "%s refs/heads/main\n", f.Commit)
	if err := os.WriteFile(filepath.Join(f.Store(), "packed-refs"), []byte(packed.String()), 0600); err != nil {
		t.Fatal(err)
	}
	refs, err := c.ListRefs(t.Context(), gittest.Repository)
	if err != nil || len(refs) != sourcecontract.MaxRefs {
		t.Fatal("cap did not accept complete inventory", len(refs), err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/overflow", f.Commit)
	if refs, err = c.ListRefs(t.Context(), gittest.Repository); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatal("truncation accepted", len(refs), err)
	}
}
