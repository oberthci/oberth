package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationTokenIsRefusedBeforeReleaseOrNetworkWork(t *testing.T) {
	for _, value := range []string{"unset", "missing", "", " \n\t"} {
		t.Run(value, func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "token")
			if value == "unset" {
				token = ""
			} else if value != "missing" {
				if err := os.WriteFile(token, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			o := options{token: token, dir: filepath.Join(t.TempDir(), "absent"), origin: ":invalid"}
			for _, operation := range []string{"publish", "finalize"} {
				err := runOperation(t.Context(), operation, o)
				if !errors.Is(err, releaseToken) || err.Error() != "Release token file is missing or empty." || releaseToken.Remedy() != "Pass --token-file with the release store token." {
					t.Fatal("token refusal came after other work or lost guidance", operation, err)
				}
			}
		})
	}
}
