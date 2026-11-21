package gitcache_test

import (
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func damageCompletionHook(t *testing.T, f *gittest.Fixture, damage string) {
	t.Helper()
	if damage == "intact" {
		return
	}
	changeReferenceHook(t, f, "missing")
	path := filepath.Join(f.Store(), "hooks", "reference-transaction")
	var err error
	switch damage {
	case "missing":
	case "empty":
		err = os.WriteFile(path, nil, 0700)
	case "not-executable":
		err = os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0600)
	case "link":
		err = os.Symlink("/bin/true", path)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "unknown":
		binary, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		body := "#!/usr/bin/python3\nimport os\nos.environ['GORACE']='atexit_sleep_ms=0'\nos.execv(" + strconv.Quote(binary) + ",['reference-transaction','future'])\n"
		err = os.WriteFile(path, []byte(body), 0700)
	default:
		t.Fatal("unknown hook fixture", damage)
	}
	if err != nil {
		t.Fatal(err)
	}
}
