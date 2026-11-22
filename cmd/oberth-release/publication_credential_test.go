package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// A by-hand publication receives the GitHub token only as a private file
// outside the release directory.
func TestPublicationTokenFileMustBePrivateAndOutsideTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		in   string
		want error
	}{
		{"private outside", 0600, "outside", nil},
		{"group readable", 0640, "outside", tokenShared},
		{"world readable", 0604, "outside", tokenShared},
		{"inside the release", 0600, "inside", tokenInside},
		{"linked from outside into the release", 0600, "link", tokenInside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			token := filepath.Join(outside, "github")
			if tc.in != "outside" {
				token = filepath.Join(dir, "github")
			}
			if err := os.WriteFile(token, []byte("fixture"), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(token, tc.mode); err != nil {
				t.Fatal(err)
			}
			if tc.in == "link" {
				link := filepath.Join(outside, "github")
				if err := os.Symlink(token, link); err != nil {
					t.Fatal(err)
				}
				token = link
			}
			_, err := publicationFor(options{token: token, dir: dir, origin: releaseverify.Origin})
			if !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
				t.Fatal("token file decision differs", err)
			}
		})
	}
}
