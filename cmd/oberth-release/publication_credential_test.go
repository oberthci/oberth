package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// Publication writes only to the canonical repository, whatever --origin says.
func TestPublicationRefusesEveryOtherOrigin(t *testing.T) {
	token := filepath.Join(t.TempDir(), "github")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://github.com/other/project/releases/download", "https://github.com/../oberth/releases/download",
		releaseverify.Origin + "/", "http://github.com/oberthci/oberth/releases/download", ""} {
		if _, err := publicationFor(options{token: token, dir: t.TempDir(), origin: origin}); !errors.Is(err, originDiffers) {
			t.Fatal("other origin accepted", origin, err)
		}
	}
	if !strings.Contains(originDiffers.Remedy(), releaseverify.Origin) {
		t.Fatal("remedy does not name the canonical address")
	}
	p, err := publicationFor(options{token: token, dir: t.TempDir(), origin: releaseverify.Origin})
	if err != nil || p.repository != "oberthci/oberth" {
		t.Fatal("canonical origin refused", err)
	}
}

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
