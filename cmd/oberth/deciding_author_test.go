package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestDecidingAuthoredPathBoundaries(t *testing.T) {
	for _, shape := range []string{"absolute", "parent", "parent-component"} {
		t.Run(shape, func(t *testing.T) {
			root := decidingDirectory(t)
			other := decidingDirectory(t)
			target := filepath.Join(other, "build.yaml")
			if err := os.WriteFile(target, []byte(starterWorkflow()), 0600); err != nil {
				t.Fatal(err)
			}
			path := target
			switch shape {
			case "parent":
				path = ".."
			case "parent-component":
				path = "../" + filepath.Base(other) + "/build.yaml"
			}
			app, _, diagnostic := fixture(t, fake.Bindings())
			app.directory = root
			if code := app.run(t.Context(), []string{"validate", path}); code != 1 {
				t.Fatal("accepted path", shape, code)
			}
			context := "File:\n" + path + ": outside the checkout\n"
			if shape == "parent" {
				context = "File: ..: outside the checkout\n"
			}
			want := "The input file or value is invalid.\nUse a readable regular file inside the checkout.\n" + context
			if diagnostic.String() != want {
				t.Fatalf("path explanation: %q want %q", diagnostic.String(), want)
			}
		})
	}
}

func TestDecidingInitReleasePreservesBuild(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = decidingDirectory(t)
	if _, err := runGit(t.Context(), app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"init"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	build := filepath.Join(app.directory, ".oberth", "build.yaml")
	const retained = "retained build workflow\n"
	if err := os.WriteFile(build, []byte(retained), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := app.run(t.Context(), []string{"init", "--release"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	data, err := os.ReadFile(build)
	if err != nil || string(data) != retained {
		t.Fatal("release initialization changed build", err)
	}
	release, err := os.ReadFile(filepath.Join(app.directory, ".oberth", "release.yaml"))
	if err != nil || string(release) != starterWorkflow() {
		t.Fatal("release starter missing", err)
	}
	want := "wrote .oberth/release.yaml (1 step)\nnext:\ngit push oberth HEAD -o oberth.run=1\n"
	if out.String() != want {
		t.Fatalf("release starter output=%q want=%q", out.String(), want)
	}
}

func TestDecidingLocalCheckPreservesFileAndReference(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true}}
	b.RunCheck.Handler = h
	app, _, diagnostic := fixture(t, b)
	app.directory = decidingDirectory(t)
	app.git = func(_ context.Context, _ []string) (string, error) { return app.directory, nil }
	const path = ".oberth/release.yaml"
	if err := os.Mkdir(filepath.Join(app.directory, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	data := starterWorkflow()
	if err := os.WriteFile(filepath.Join(app.directory, path), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"check", "refs/tags/v0.1.0", "--repo", "source/team/widget", "--local", path}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	inputs, _, calls := h.Observed()
	if calls != 1 || inputs[0].Repo != "source/team/widget" || inputs[0].Ref != "refs/tags/v0.1.0" || len(inputs[0].Files) != 1 || inputs[0].Files[0].Path != path || inputs[0].Files[0].Content != data {
		t.Fatalf("local check changed authored input: %+v", inputs)
	}
}

func decidingDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}
