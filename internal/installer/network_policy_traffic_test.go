package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyTrafficScriptDistinguishesTransportFailures(t *testing.T) {
	dir := t.TempDir()
	wget := `#!/bin/sh
while [ "$1" != "-O" ]; do shift; done
shift
printf '%s' "$PROBE_BODY" > "$1"
printf '%s' "$PROBE_ERROR" >&2
exit "$PROBE_EXIT"
`
	if err := os.WriteFile(filepath.Join(dir, "wget"), []byte(wget), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body, diagnostic, exit, want string }{
		{"allowed", "oberth-policy-probe", "", "0", "allowed"},
		{"drop", "", "wget: download timed out", "1", "timeout"},
		{"refused", "", "wget: can't connect to remote host: Connection refused", "1", "refused"},
		{"reset", "", "wget: error getting response: Connection reset by peer", "1", "reset"},
		{"http error", "", "HTTP/1.1 503 Service Unavailable: Connection refused", "1", "invalid"},
		{"wrong body", "another server", "", "0", "invalid"},
		{"unknown", "", "private diagnostic", "1", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.ReplaceAll(policyTrafficScript, "/probe/", dir+"/")
			command := exec.CommandContext(t.Context(), "sh", "-c", script, "sh", "http://192.0.2.10:8080/")
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "PROBE_BODY="+tc.body, "PROBE_ERROR="+tc.diagnostic, "PROBE_EXIT="+tc.exit)
			output, err := command.CombinedOutput()
			if err != nil || string(output) != tc.want {
				t.Fatalf("got %q %v, want %q", output, err, tc.want)
			}
		})
	}
}
