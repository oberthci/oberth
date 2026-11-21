package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	chartfake "github.com/oberthci/oberth/internal/installer/fake"
)

func TestInstallFromAbsentConfigurationBase(t *testing.T) {
	for _, mode := range []string{"apply", "dry-run", "decline", "refuse-after-approval"} {
		t.Run(mode, func(t *testing.T) { installFromAbsentBase(t, mode) })
	}
}
func installFromAbsentBase(t *testing.T, mode string) {
	t.Helper()
	a, charts, diagnostic := freshBaseApplication(t)
	refused := false
	if mode == "refuse-after-approval" {
		a.out = refuseApprovedInstall{Writer: a.out, refuse: func() {
			refused = true
			assertPrivateInstallDirectory(t, a.base)
		}}
	}
	code := a.run(t.Context(), freshBaseArguments(mode))
	if mode == "apply" {
		if code != 0 || len(charts.Applies) != 1 {
			t.Fatal("fresh approved install failed", code, diagnostic, len(charts.Applies))
		}
		assertPublishedInstallation(t, a.base)
		return
	}
	want := 0
	if mode == "refuse-after-approval" {
		want = 1
	}
	if code != want || refused != (mode == "refuse-after-approval") || len(charts.Applies) != 0 {
		t.Fatal("unexpected installation outcome", code, refused, diagnostic)
	}
	if _, err := os.Lstat(filepath.Dir(a.base)); !os.IsNotExist(err) {
		t.Fatal("unapplied install left configuration directories", err)
	}
}

type refuseApprovedInstall struct {
	io.Writer
	refuse func()
}

func (w refuseApprovedInstall) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("checking the cluster")) {
		w.refuse()
		return 0, errors.New("injected refusal after approval")
	}
	return w.Writer.Write(p)
}
func assertPrivateInstallDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("configuration directory is not private", path, info.Mode())
	}
}

func freshBaseApplication(t *testing.T) (application, *chartfake.Chart, *bytes.Buffer) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	a, _, diagnostic := fixture(t, fake.Bindings())
	parent := filepath.Join(home, ".config")
	a.base = filepath.Join(parent, "oberth")
	engine, charts := offlineInstallEngine(t, a.base)
	a.in = strings.NewReader("n\n")
	a.install = func(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
		s, err := a.openInstallation(r)
		if err != nil {
			return err
		}
		credentials, err := installer.OpenConfigDir(filepath.Join(a.base, s.name))
		if err != nil {
			return err
		}
		defer credentials.Close()
		copied := *engine
		copied.Credentials = credentials
		s.engine, s.input = &copied, bufio.NewScanner(in)
		return s.dispatch(ctx, in, out)
	}
	return a, charts, diagnostic
}

func freshBaseArguments(mode string) []string {
	args := []string{"install", "--context", "chosen", "--chart", "../../charts/oberth"}
	if mode == "dry-run" {
		args = append(args, "--dry-run")
	}
	if mode == "apply" || mode == "refuse-after-approval" {
		args = append(args, "--yes")
	}
	return args
}

func assertPublishedInstallation(t *testing.T, base string) {
	t.Helper()
	profile, dir, err := client.SelectedProfile(base, "")
	if err != nil || profile.ServerURL == "" {
		t.Fatal("installation profile was not published/selected", profile, err)
	}
	for _, path := range []string{filepath.Dir(base), base, dir} {
		assertPrivateInstallDirectory(t, path)
	}
}
