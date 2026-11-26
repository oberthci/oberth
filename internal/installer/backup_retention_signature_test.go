package installer

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetentionRequiresVerifiedFullManifestForThisInstallation(t *testing.T) {
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"signed-full", "missing", "junk", "bad-signature", "signed-partial", "wrong-name", "wrong-id"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			name := "oberth-backup-20261101T000000.000000000Z.age"
			path := filepath.Join(dir, name)
			c := BackupConfig{signing: private, Installation: true, manifest: &bundleManifest{}}
			writeRetentionProof(t, c, path, mode)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			names, err := c.retainedArchives(root)
			if err != nil || len(names) != map[bool]int{true: 1, false: 0}[mode == "signed-full"] {
				t.Fatal("retention counted an unverified or partial manifest", mode, names, err)
			}
			if err = pruneBackups(root, names, 1); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(path)
			if (err == nil) == (mode == "signed-full") {
				t.Fatal("retention deleted an ineligible archive or kept an eligible one", mode)
			}
		})
	}
}

func writeRetentionProof(t *testing.T, c BackupConfig, path, mode string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("archive-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "missing" {
		return
	}
	origin := backupOrigin{InstallationID: c.installationID(), Full: mode != "signed-partial", Name: filepath.Base(path), SHA256: strings.Repeat("a", 64)}
	if mode == "wrong-name" {
		origin.Name = "another.age"
	}
	if mode == "wrong-id" {
		origin.InstallationID = base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	}
	raw, _ := json.Marshal(origin)
	signature := ed25519.Sign(c.signing, append([]byte(backupOriginDomain), raw...))
	if mode == "bad-signature" {
		signature[0] ^= 1
	}
	proof, _ := json.Marshal(signedBackupOrigin{Origin: origin, Signature: signature})
	if mode == "junk" {
		proof = []byte("not a signed manifest")
	}
	if err := os.WriteFile(path+".manifest.json", proof, 0600); err != nil {
		t.Fatal(err)
	}
}
