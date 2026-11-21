package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestInstallDeclineReportsNoChange(t *testing.T) {
	engine, charts := offlineInstallEngine(t, t.TempDir())
	flags := installationFlags("install")
	if err := flags.Parse([]string{"--context=chosen", "--chart=../../charts/oberth"}); err != nil {
		t.Fatal(err)
	}
	request := installationRequest{name: "install", flags: flags}
	s := installationSession{engine: engine, request: request, choices: request.choices()}
	var out bytes.Buffer
	if err := s.applyInstallation(t.Context(), strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "\nNot applied; nothing was changed.\n") || strings.Contains(out.String(), "agent tools") || len(charts.Applies) != 0 {
		t.Fatal(out.String())
	}
}

func TestInstallDispatchDoesNotInventAnApply(t *testing.T) {
	a, out, diagnostic := fixture(t, fake.Bindings())
	a.install = func(context.Context, installationRequest, io.Reader, io.Writer) error { return nil }
	if code := a.run(t.Context(), []string{"install", "--verify"}); code != 0 || out.Len() != 0 {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

func TestInstallApplyPrintsAgentHint(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	engine, charts := offlineInstallEngine(t, base)
	flags := installationFlags("install")
	if err := flags.Parse([]string{"--context=chosen", "--chart=../../charts/oberth", "--yes"}); err != nil {
		t.Fatal(err)
	}
	request := installationRequest{name: "install", flags: flags}
	s := installationSession{app: application{base: base}, name: "installed", engine: engine, request: request, choices: request.choices()}
	var out bytes.Buffer
	if err := s.applyInstallation(t.Context(), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	assertInstallationCredentialHeadings(t, out.String())
	if !strings.Contains(out.String(), "agent tools: run oberth join") || strings.Contains(out.String(), " mcp") || len(charts.Applies) == 0 {
		t.Fatal(out.String())
	}
}
