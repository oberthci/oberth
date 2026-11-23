//go:build linux

package gitcache_test

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

// Two fixtures initialize together, as in the random-stop batches. Each writes
// executable wrappers while the other can fork a Git command.
func TestReceiveRandomStopFixtureInit(t *testing.T) {
	for i := range 2 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			f := gittest.New(t)
			control := rsControl(t, f, false)
			cfg := rsConfig(f, control)
			cfg.Log = func(reason, detail string) { t.Logf("%s: %s", reason, detail) }
			c := rsOpen(t, cfg)
			if err := c.Init(t.Context(), gittest.Repository); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// rsControl writes the run's wrappers, killer and (for native stops) the
// package's rename fault library.
func rsControl(t *testing.T, f *gittest.Fixture, native bool) string {
	t.Helper()
	control := filepath.Join(f.Root, "ctl")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"git": rsGitWrapper(control, f.Binary), "hook": rsHookWrapper(control, binary), "killer.py": rsKiller}
	if native {
		image, err := nativeFaultImage()
		if err != nil {
			t.Fatal(err)
		}
		files["fault.so"] = string(image)
	}
	if err = os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	// A parallel fork must not inherit an executable's writable descriptor:
	// it can keep the file busy until exec, after WriteFile has closed it here.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	for name, body := range files {
		if err = os.WriteFile(filepath.Join(control, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return control
}
