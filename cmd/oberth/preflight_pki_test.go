package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPKIPreflightHelpAndUpgradeTLSFlags(t *testing.T) {
	for _, command := range []string{"preflight", "upgrade"} {
		var output bytes.Buffer
		var err error
		if command == "preflight" {
			err = runPreflight(context.Background(), []string{"pki", "--help"}, &output)
		} else {
			err = runUpgrade(context.Background(), []string{"--help"}, nil, &output)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"-namespace", "-context", "-openbao-namespace", "-tls-extra-dns-name", "-tls-extra-ip"} {
			if !strings.Contains(output.String(), flag) {
				t.Fatalf("%s help lacks %s: %s", command, flag, output.String())
			}
		}
		if command == "upgrade" && (!strings.Contains(output.String(), "-tls-remove-dns-name") || !strings.Contains(output.String(), "-tls-remove-ip")) {
			t.Fatalf("upgrade lacks explicit removal flags: %s", output.String())
		}
	}
}
