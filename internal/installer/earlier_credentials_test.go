package installer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// Failed helper repairs keep every earlier generation; the next successful one
// removes them all except what the helper names.
func TestSuccessfulRepairRemovesEveryKeptGeneration(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.engine.GitHelper = func(context.Context, installer.Profile) ([]string, bool) { return nil, false }
	for range 3 {
		apply(t, f, prepare(t, f))
	}
	if keys := administratorKeys(t, f.dir); keys != 4 {
		t.Fatal("failed repairs did not keep the earlier keys", keys)
	}
	helperHosts := filepath.Join(f.dir, "known_hosts-"+strings.Repeat("a", 64))
	if err := os.WriteFile(helperHosts, []byte("[127.0.0.1]:22 key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.engine.GitHelper = func(_ context.Context, p installer.Profile) ([]string, bool) {
		return []string{p.AdminKey, helperHosts}, true
	}
	apply(t, f, prepare(t, f))
	if keys := administratorKeys(t, f.dir); keys != 1 {
		t.Fatal("successful repair kept earlier keys", keys)
	}
	earlier, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range earlier {
		if strings.HasPrefix(entry.Name(), "admin-") || strings.HasPrefix(entry.Name(), "ca-") {
			continue
		}
		if strings.HasPrefix(entry.Name(), "known_hosts-") && filepath.Join(f.dir, entry.Name()) == helperHosts {
			return
		}
	}
	t.Fatal("prune removed the host-key file the helper names")
}

func administratorKeys(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "admin-") && strings.HasSuffix(entry.Name(), ".key") {
			keys++
		}
	}
	return keys
}
