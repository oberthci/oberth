package main

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoAddBindsServerReturnedName(t *testing.T) {
	b := fake.Bindings()
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "canonical/team/project"}}
	verified := &fake.Handler[apicontract.RepoVerifyArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "canonical/team/project"}}
	b.RepoVerify.Handler = verified
	app, out, diagnostic := fixture(t, b)
	_, metadata := joinAdministratorProfile(t, &app, "install")
	app.directory = t.TempDir()
	if _, err := runGit(t.Context(), app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.git = func(ctx context.Context, args []string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return runGit(ctx, app.directory, args...)
	}
	if code := app.run(t.Context(), []string{"repo", "add", "https://forge.example/team/project.git", "--known-hosts", metadata.KnownHosts}); code != 0 {
		t.Fatalf("repo add failed: %s", diagnostic.String())
	}
	inputs, _, _ := verified.Observed()
	if len(inputs) != 1 || inputs[0].Repo != "canonical/team/project" {
		t.Fatal("verification used computed name")
	}
	if !strings.Contains(strings.Join(calls, "\n"), "config oberth.repository canonical/team/project") || !strings.Contains(strings.Join(calls, "\n"), "canonical/team/project.git") {
		t.Fatal("checkout did not bind returned name", calls)
	}
	if !strings.Contains(out.String(), "bound:\ncanonical/team/project") || !strings.Contains(out.String(), "HEAD:refs/heads/<new-branch> -o oberth.run=1") {
		t.Fatal("wrong receipt or push suggestion", out.String())
	}
}
func TestKnownHostsRefusalNamesExternalPath(t *testing.T) {
	for _, kind := range []string{"missing", "writable", "directory", "link"} {
		t.Run(kind, func(t *testing.T) { knownHostCase(t, kind) })
	}
}
func knownHostCase(t *testing.T, kind string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	switch kind {
	case "writable":
		if err := os.WriteFile(path, []byte("host key"), 0666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	case "link":
		if err := os.Symlink("absent", path); err != nil {
			t.Fatal(err)
		}
	}
	err := checkHostFile(path)
	if err == nil {
		t.Fatal("unsafe host file accepted")
	}
	app, _, diag := fixture(t, fake.Bindings())
	app.failed(err)
	if !strings.Contains(diag.String(), path) || strings.Contains(diag.String(), "inside the checkout") {
		t.Fatal("host-key remedy wrong", diag.String())
	}
}
func TestGitFailureNamesOperationAndRetainsDetail(t *testing.T) {
	_, err := exec.CommandContext(t.Context(), "sh", "-c", "exit 128").Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("exit fixture did not fail", err)
	}
	exit.Stderr = []byte("fatal: cannot fetch remote\ncomplete native detail")
	for _, operation := range []string{"clone", "fetch", "push", "tag"} {
		t.Run(operation, func(t *testing.T) {
			app, _, diag := fixture(t, fake.Bindings())
			s := flowSession{app: app, options: commonOptions{detail: true}}
			s.failed(gitFailure([]string{operation}, err))
			if !strings.Contains(strings.ToLower(diag.String()), operation) || !strings.Contains(diag.String(), "fatal: cannot fetch remote") || !strings.Contains(diag.String(), "complete native detail") {
				t.Fatal("Git failure lost useful detail", diag.String())
			}
		})
	}
	exit.Stderr = []byte("Host key verification failed.")
	app, _, diag := fixture(t, fake.Bindings())
	app.failed(gitFailure([]string{"fetch"}, err))
	if !strings.Contains(diag.String(), "SSH host-key verification failed") {
		t.Fatal("host-key failure hidden", diag.String())
	}
}
func TestInvalidReleaseTagNamesInput(t *testing.T) {
	f := newReleaseFixture(t)
	if f.app.run(t.Context(), []string{"release", "v0..1"}) != 1 || !strings.Contains(f.diagnostic.String(), "tag name is invalid") || !strings.Contains(f.diagnostic.String(), "v0..1") {
		t.Fatal("invalid tag not explained", f.diagnostic.String())
	}
	assertNoReleaseEffects(t, f)
}
