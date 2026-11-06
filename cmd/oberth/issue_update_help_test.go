package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type issueHelpInput struct{ t *testing.T }

func (input issueHelpInput) Read([]byte) (int, error) {
	input.t.Error("help read command input")
	return 0, io.ErrUnexpectedEOF
}

func TestIssuesUpdateHelpIsLocal(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "unconfigured"
		if configured {
			name = "unavailable credentials and server"
		}
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT"} {
				t.Setenv(key, "")
			}
			if configured {
				t.Setenv("OBERTH_BASE_URL", "https://127.0.0.1:1")
				t.Setenv("OBERTH_MCP_URL", "https://127.0.0.1:1/mcp")
				t.Setenv("OBERTH_TOKEN_COMMAND", filepath.Join(t.TempDir(), "missing-credential-command"))
				t.Setenv("OBERTH_CA_CERT", filepath.Join(t.TempDir(), "missing-ca.pem"))
			}
			for _, flag := range []string{"--help", "-h"} {
				var out strings.Builder
				if err := runCLI(t.Context(), []string{"issues", "update", flag}, issueHelpInput{t}, &out); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"Usage: oberth issues update", "-id", "-work-status", "-expected-revision"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing %q: %s", want, &out)
					}
				}
				if strings.Contains(out.String(), "-stale-after") {
					t.Error("update printed list help")
				}
			}
		})
	}
}
