package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/publicerr"
)

const backupOriginDomain = "oberth-backup-manifest/v1\n"

// backupOrigin binds the archive's own file name, which carries its time in a
// backup directory, so a genuine older archive cannot pass as a newer name.
type backupOrigin struct {
	Name     string         `json:"archive_name"`
	SHA256   string         `json:"archive_sha256"`
	Manifest bundleManifest `json:"manifest"`
}

type signedBackupOrigin struct {
	Origin    backupOrigin `json:"origin"`
	Signature []byte       `json:"signature"`
}

func verifyWrittenArchive(ctx context.Context, file *os.File, expected string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return recoveryProblem()
	}
	header := make([]byte, len("age-encryption.org/v1\n"))
	if _, err := io.ReadFull(file, header); err != nil || string(header) != "age-encryption.org/v1\n" {
		return recoveryProblem()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return recoveryProblem()
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, archiveReader{ctx: ctx, in: file}); err != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
		return recoveryProblem()
	}
	return nil
}

func (c BackupConfig) publishOrigin(archive Archive) error {
	if len(c.signing) != ed25519.PrivateKeySize || c.manifest == nil {
		return backupSigningProblem()
	}
	origin := backupOrigin{Name: filepath.Base(archive.Path), SHA256: archive.SHA256, Manifest: *c.manifest}
	raw, err := json.Marshal(origin)
	if err != nil {
		return backupSigningProblem()
	}
	proof, err := json.Marshal(signedBackupOrigin{Origin: origin, Signature: ed25519.Sign(c.signing, append([]byte(backupOriginDomain), raw...))})
	if err != nil {
		return backupSigningProblem()
	}
	path := archive.Path + ".manifest.json"
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return backupSigningProblem()
	}
	identity, statErr := file.Stat()
	_, writeErr := file.Write(proof)
	err = errors.Join(statErr, writeErr, file.Sync(), file.Close())
	if err != nil {
		_ = removeBackupStage(identity, &path)
		return backupSigningProblem()
	}
	if err = syncParent(archive.Path); err != nil {
		return backupSigningProblem()
	}
	return nil
}

func archiveOrigin(ctx context.Context, file *os.File, config RestoreReviewConfig) (string, error) {
	limit, _ := config.Options.Limit()
	maximum := limit + (2 << 20) // Multipart framing and age's bounded per-chunk overhead.
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(archiveReader{ctx: ctx, in: file}, maximum+1))
	if err != nil || n > maximum {
		return "", backupOriginProblem()
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if config.ExpectedSHA256 != "" {
		if config.ExpectedSHA256 != digest {
			return "", backupOriginProblem()
		}
	} else if err = verifyOriginProof(config, digest); err != nil {
		return "", err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return "", restoreProblem()
	}
	return digest, nil
}

func verifyOriginProof(config RestoreReviewConfig, digest string) error {
	public, err := base64.StdEncoding.DecodeString(config.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return backupOriginProblem()
	}
	proofFile, err := openBackupArchive(config.Path + ".manifest.json")
	if err != nil {
		return backupOriginProblem()
	}
	defer proofFile.Close()
	raw, err := io.ReadAll(io.LimitReader(proofFile, (1<<20)+1))
	var proof signedBackupOrigin
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || len(raw) > 1<<20 || decoder.Decode(&proof) != nil || decoder.Decode(new(any)) != io.EOF || proof.Origin.SHA256 != digest || proof.Origin.Name != filepath.Base(config.Path) {
		return backupOriginProblem()
	}
	message, err := json.Marshal(proof.Origin)
	if err != nil || !ed25519.Verify(public, append([]byte(backupOriginDomain), message...), proof.Signature) {
		return backupOriginProblem()
	}
	return nil
}

func backupOriginProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Backup origin proof is missing or invalid.", "Keep the archive's original name; use the saved backup profile or an independently saved --sha256 digest.", nil)
}

func backupSigningProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Backup signing could not finish.", "Check the backup account's signing Secret and backup directory; keep operator keys off-cluster.", nil)
}
