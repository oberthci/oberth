package store

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// The forge host pins beside the database travel in its archive, bound by the
// manifest, and a restore writes them back beside the restored database.
func TestArchiveCarriesHostPinsBoundByTheManifest(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	pins := []byte("[forge.oberth.ci]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPinFixture\n")
	requireOK(t, os.MkdirAll(filepath.Dir(HostPinsPath(s.path)), 0700))
	requireOK(t, os.WriteFile(HostPinsPath(s.path), pins, 0600))
	var archive bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = backupForTest(t, s).Restore(t.Context(), stateport.RestoreRequest{Options: backupOptions(s), Destination: destination}, bytes.NewReader(archive.Bytes()))
	requireOK(t, err)
	restored, err := os.ReadFile(HostPinsPath(destination))
	if err != nil || !bytes.Equal(restored, pins) {
		t.Fatal("restore did not write back the archived host pins", err)
	}
	altered := alteredArchive(t, archive.Bytes(), func(h *tar.Header, data []byte) []byte {
		if h.Name == hostPinsName {
			return append(bytes.Clone(data), "extra.example ssh-ed25519 AAAA\n"...)
		}
		return data
	})
	_, err = backupForTest(t, s).Inspect(t.Context(), backupOptions(s), bytes.NewReader(altered))
	requireCode(t, err, publicerr.SchemaRefused)
}
