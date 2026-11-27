package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestPushDiagnosticsStayOnTheClientSideOfTheForgeBoundary(t *testing.T) {
	raw := gitssh.Run(t.Context(), gitssh.Config{Command: "git-receive-pack 'r.git'"}, gitssh.IO{})
	for _, forge := range []bool{false, true} {
		var diagnostic bytes.Buffer
		a := application{in: strings.NewReader(""), out: io.Discard, diagnostic: &diagnostic, env: func(name string) string {
			if forge && name == gitssh.ConfigEnvironment {
				return "selected-config"
			}
			return ""
		}}
		got := a.gitSSHProblem(raw)
		if forge {
			if !errors.Is(got, raw) || publicerr.Classify(got).Reason() != publicerr.Classify(raw).Reason() {
				t.Fatal("forge transport was relabelled", got)
			}
		} else if publicerr.Classify(got).Reason() != gitssh.PushLocalRefusal {
			t.Fatal("client lost its preconnection evidence", got)
		}
		code := a.gitSSH(t.Context(), []string{"--config", filepath.Join(t.TempDir(), "absent-config"), "git@fixture", "git-receive-pack 'r.git'"})
		if code == 0 || strings.Contains(diagnostic.String(), gitssh.PushLocalRefusal) == forge {
			t.Fatal("installed shim crossed client/forge diagnostic scope", forge, code, diagnostic.String())
		}
	}
}
