package releaseimage

import (
	"archive/tar"
	"testing"
	"time"
)

func TestScratchContractRefusesExtraPayloadAndAliasChanges(t *testing.T) {
	created := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"bin/sh", "usr/bin/ssh", "usr/bin/curl", "lib/libc.so", "lib/apk/db/installed", "unexpected"} {
		t.Run(name, func(t *testing.T) {
			base := testBaseImage(t, created, map[string]testEntry{
				"usr/local/bin/oberth": {body: "old", mode: 0755}, "usr/bin/git": {body: "git", mode: 0755}, "etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0644}, "usr/local/bin/oberth-git-ssh": {link: "oberth", mode: 0777}, name: {body: "extra", mode: 0755},
			})
			if _, err := Repack(base, Server, "amd64", []byte("new"), created, "test", "rev"); err == nil {
				t.Fatal("extra payload admitted")
			}
		})
	}
	for _, link := range []string{"/usr/bin/ssh", "../../bin/sh", "/usr/local/bin/oberth", "other"} {
		h := &tar.Header{Name: "usr/local/bin/oberth-git-ssh", Typeflag: tar.TypeSymlink, Linkname: link, Mode: 0777}
		if err := validateRuntimeEntry(h, h.Name); err == nil {
			t.Fatalf("alias %q admitted", link)
		}
	}
}
