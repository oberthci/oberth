package main

import (
	"bytes"
	"testing"
)

func TestUnsealHelpAndRecoveryGates(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--status", "--secrets-file", "file"}, {"--seal-to-host", "file", "--secrets-file", "recovery"}, nil} {
		var output bytes.Buffer
		err := runUnseal(t.Context(), args, &output)
		help := len(args) > 0 && args[0] == "--help"
		if (err == nil) != help {
			t.Fatalf("unexpected argument validation for %v", args)
		}
		if help && !bytes.Contains(output.Bytes(), []byte("anyone booting its disk here gets the secrets")) {
			t.Fatal("help omits host-binding risk")
		}
	}
}
