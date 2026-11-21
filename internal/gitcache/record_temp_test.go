package gitcache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestStartupRemovesUnownedRecordTemps(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := thirdInit(t, cfg)
	paths := []string{".oberth-record-interrupted", ".oberth-record-another",
		"oberth-receive.json", "objects/.oberth-record-unrelated"}
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(f.Store(), path), []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if other, err := gitcache.New(t.Context(), cfg); err == nil {
		_ = other.Close()
		t.Fatal("startup entered a live writer's cache")
	}
	if _, err := os.Stat(filepath.Join(f.Store(), paths[0])); err != nil {
		t.Fatal("startup removed a live owner's temporary record", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	_ = reviewNew(t, cfg)
	for i, path := range paths {
		data, err := os.ReadFile(filepath.Join(f.Store(), path))
		if i < 2 {
			if !os.IsNotExist(err) {
				t.Fatal("startup retained an abandoned temporary record", path, err)
			}
			continue
		}
		if err != nil || string(data) != path {
			t.Fatal("startup changed durable evidence or an unrelated file", path, err)
		}
	}
}
