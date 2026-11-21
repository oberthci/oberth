package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

const initRetainedSibling = "retained authored file\n"

func TestInitForceRefusesSelectedLinks(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		for _, kind := range []string{"relative", "absolute", "dangling"} {
			t.Run(name+"/"+kind, func(t *testing.T) { checkInitForceLink(t, name, kind) })
		}
	}
}

func checkInitForceLink(t *testing.T, name, kind string) {
	t.Helper()
	app, out, diagnostic := initForceCheckout(t)
	directory := filepath.Join(app.directory, ".oberth")
	sibling := filepath.Join(directory, "retained.yaml")
	if err := os.WriteFile(sibling, []byte(initRetainedSibling), 0600); err != nil {
		t.Fatal(err)
	}
	target := "retained.yaml"
	if kind == "absolute" {
		target = sibling
	}
	if kind == "dangling" {
		target = "absent.yaml"
	}
	selected := filepath.Join(directory, name)
	if err := os.Symlink(target, selected); err != nil {
		t.Fatal(err)
	}
	code := app.run(t.Context(), initForceArgs(name))
	assertInitSibling(t, sibling)
	if code != 1 || out.Len() != 0 {
		t.Fatalf("selected link accepted: exit=%d output=%q", code, out.String())
	}
	want := "The pipeline path is not a regular file.\nMove the named path aside, then rerun oberth init.\nFile:\n" + selected + "\n"
	if diagnostic.String() != want {
		t.Fatalf("refusal=%q want=%q", diagnostic.String(), want)
	}
	if actual, err := os.Readlink(selected); err != nil || actual != target {
		t.Fatal("refusal replaced selected link", actual, err)
	}
	assertInitEntries(t, directory)
}

func TestInitForceReplacesOnlySelectedFile(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		for _, kind := range []string{"regular", "absent", "hardlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) { checkInitForceReplacement(t, name, kind) })
		}
	}
}

func checkInitForceReplacement(t *testing.T, name, kind string) {
	t.Helper()
	app, out, diagnostic := initForceCheckout(t)
	directory := filepath.Join(app.directory, ".oberth")
	sibling := filepath.Join(directory, "retained.yaml")
	if err := os.WriteFile(sibling, []byte(initRetainedSibling), 0600); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(directory, name)
	prepareInitSelected(t, selected, sibling, kind)
	if code := app.run(t.Context(), initForceArgs(name)); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	data, err := os.ReadFile(selected)
	if err != nil || string(data) != starterWorkflow() {
		t.Fatal("selected workflow missing", err)
	}
	assertInitSibling(t, sibling)
	info, err := os.Stat(selected)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "absent" && info.Mode().Perm() != 0600 {
		t.Fatal("force changed selected file permissions", info.Mode())
	}
	siblingInfo, err := os.Stat(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(info, siblingInfo) {
		t.Fatal("selected file still shares sibling storage")
	}
	want := "wrote .oberth/" + name + " (1 step)\nnext:\ngit push oberth HEAD -o oberth.run=1\n"
	if out.String() != want {
		t.Fatalf("output=%q want=%q", out.String(), want)
	}
	assertInitEntries(t, directory)
}

func initForceCheckout(t *testing.T) (application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	if err := os.Chmod(app.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(t.Context(), app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(app.directory, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	return app, out, diagnostic
}

func initForceArgs(name string) []string {
	args := []string{"init", "--force"}
	if name == "release.yaml" {
		args = append(args, "--release")
	}
	return args
}

func prepareInitSelected(t *testing.T, selected, sibling, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "regular":
		err = os.WriteFile(selected, []byte("old workflow\n"), 0600)
	case "hardlink":
		err = os.Link(sibling, selected)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertInitSibling(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != initRetainedSibling {
		t.Fatal("force changed sibling", err)
	}
}

func assertInitEntries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatal("init created an unexpected target or temporary file", entries, err)
	}
}
