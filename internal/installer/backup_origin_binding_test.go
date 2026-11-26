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

func TestOriginVerificationUsesSignatureAndArchiveBinding(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.age")
	digest := strings.Repeat("a", 64)
	proof := signedBackupOrigin{Origin: backupOrigin{Name: filepath.Base(path), SHA256: digest}}
	message, _ := json.Marshal(proof.Origin)
	proof.Signature = ed25519.Sign(private, append([]byte(backupOriginDomain), message...))
	raw, _ := json.Marshal(proof)
	if err = os.WriteFile(path+".manifest.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	config := RestoreReviewConfig{Path: path, PublicKey: base64.StdEncoding.EncodeToString(public)}
	if err = verifyOriginProof(config, digest); err != nil {
		t.Fatal("valid archive authentication depended on retention metadata", err)
	}
	if err = verifyOriginProof(config, strings.Repeat("b", 64)); err == nil {
		t.Fatal("archive authentication ignored its digest binding")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, eligible := retainedOrigin(root, filepath.Base(path)); eligible {
		t.Fatal("archive authentication granted retention without full/installation metadata")
	}
}
