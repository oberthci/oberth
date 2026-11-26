package store

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestUnsupportedArchiveMemberNamesReleaseRemedy(t *testing.T) {
	s := testStore(t)
	var body bytes.Buffer
	archive := tar.NewWriter(&body)
	requireOK(t, writeMember(archive, "oberth.sqlite", []byte("fixture")))
	requireOK(t, writeMember(archive, "future-host-pins", []byte("public-pin-fixture")))
	requireOK(t, archive.Close())
	_, err := backupForTest(t, s).Inspect(t.Context(), backupOptions(s), &body)
	e := publicerr.Classify(err)
	if err == nil || e.Code() != publicerr.SchemaRefused || !strings.Contains(strings.ToLower(e.Remedy()), "restore with the same or a newer release") {
		t.Fatal("unsupported archive omitted its version remedy", err)
	}
}
