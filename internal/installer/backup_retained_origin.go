package installer

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
)

func (c BackupConfig) installationID() string {
	return base64.StdEncoding.EncodeToString(c.signing.Public().(ed25519.PublicKey))
}

// A sidecar counts only when its signature, full flag, name and signer ID
// agree. A different signer identifies another installation in a shared mount.
func retainedOrigin(root *os.Root, archive string) (signedBackupOrigin, bool) {
	file, err := root.OpenFile(archive+".manifest.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return signedBackupOrigin{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return signedBackupOrigin{}, false
	}
	proof, err := readBackupOrigin(file)
	if err != nil {
		return proof, false
	}
	public, err := base64.StdEncoding.DecodeString(proof.Origin.InstallationID)
	return proof, err == nil && proof.Origin.Full && proof.Origin.InstallationID == base64.StdEncoding.EncodeToString(public) && proof.valid(public, archive)
}

func sharedBackupDirectoryProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Backups belong to another installation.", "Choose a separate directory, or use --allow-shared-directory to prune only this installation's signed full archives.", nil)
}
