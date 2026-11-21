package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func baseGitOptions(t *testing.T) options {
	t.Helper()
	dir := t.TempDir()
	o := options{base: filepath.Join(dir, "base"), pin: filepath.Join(dir, "base.digest"), git: fixturePin, gitOut: filepath.Join(dir, "git")}
	setupBase(t, o)
	return o
}

func TestBaseExportsThisMachinesCheckedGit(t *testing.T) {
	o := baseGitOptions(t)
	if err := checkBase(o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.gitOut, "git")
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, fixtureGit(runtime.GOARCH)) {
		t.Fatal("exported Git differs from the base Git for this machine", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("exported Git is not executable", err)
	}
}

func TestBaseExportRefusesBeforeWriting(t *testing.T) {
	for _, state := range []string{"pin", "existing"} {
		t.Run(state, func(t *testing.T) {
			o := baseGitOptions(t)
			var want error = baseMismatch
			if state == "pin" {
				if err := os.WriteFile(o.pin, []byte("sha256:"+strings.Repeat("0", 64)+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				want = os.ErrExist
				if err := os.Mkdir(o.gitOut, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkBase(o); !errors.Is(err, want) {
				t.Fatal("base export did not refuse", err)
			}
			if _, err := os.Stat(filepath.Join(o.gitOut, "git")); !os.IsNotExist(err) {
				t.Fatal("refused export wrote Git", err)
			}
		})
	}
}

func TestBaseCheckWithoutGitOutWritesNothing(t *testing.T) {
	o := baseGitOptions(t)
	o.gitOut = ""
	if err := checkBase(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(o.base), "git")); !os.IsNotExist(err) {
		t.Fatal("check without an output wrote Git", err)
	}
}
