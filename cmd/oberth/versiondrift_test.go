package main

import (
	"bytes"
	"strings"
	"testing"
)

// An older CLI against a newer server sends requests the server reads
// differently and omits flags that exist now, and every symptom of it reads as
// the server misbehaving. Status is the command someone runs when something is
// odd, and it already holds both versions.
func TestStatusWarnsWhenTheCLIIsOlderThanTheServer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		cli        string
		server     string
		wantWarned bool
	}{
		{name: "older CLI", cli: "0.17.0", server: "0.18.0", wantWarned: true},
		{name: "older CLI with v prefix", cli: "v0.17.0", server: "v0.18.2", wantWarned: true},
		{name: "same version", cli: "0.18.0", server: "0.18.0"},
		// The ordinary state during a rollout, and nothing to say about it.
		{name: "newer CLI", cli: "0.19.0", server: "0.18.0"},
		// A development build is not on this scale, and a guess about which
		// side is older would be worse than silence.
		{name: "dev build", cli: "dev", server: "0.18.0"},
		{name: "server did not say", cli: "0.17.0", server: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			warnVersionDrift(&out, test.cli, test.server)
			warned := strings.Contains(out.String(), "Warning")
			if warned != test.wantWarned {
				t.Fatalf("warned = %v, want %v (output %q)", warned, test.wantWarned, out.String())
			}
			if test.wantWarned && !strings.Contains(out.String(), test.server) {
				t.Fatalf("the warning does not name the server version: %q", out.String())
			}
		})
	}
}
