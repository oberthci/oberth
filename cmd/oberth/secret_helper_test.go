package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableInstallCopiesOnlyProgramWithoutProfile(t *testing.T) {
	t.Parallel()
	binary := composeBinary(t)
	directory := t.TempDir()
	helper := filepath.Join(directory, "oberth")
	out, detail, code := heldExecutable(t, binary, []string{"internal-install", "--to", helper})
	if code != 0 || out != "" || detail != "" {
		t.Fatal("program copy failed", code, out, detail)
	}
	checkInstalledExecutable(t, binary, helper)
	for _, args := range [][]string{{"internal-install"}, {"internal-install", "--wrong", helper}, {"internal-install", "--to", filepath.Join(directory, "missing", "oberth")}} {
		_, _, code = heldExecutable(t, binary, args)
		if code == 0 {
			t.Fatal("invalid installation succeeded", args)
		}
	}
}

func checkInstalledExecutable(t *testing.T, binary, helper string) {
	t.Helper()
	directory := filepath.Dir(helper)
	want, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(helper)
	if err != nil || !bytes.Equal(want, got) {
		t.Fatal("program bytes differ", err)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || files[0].Name() != "oberth" {
		t.Fatal("copy carried other files", files, err)
	}
	info, err := os.Stat(helper)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("program mode differs", info, err)
	}
}
