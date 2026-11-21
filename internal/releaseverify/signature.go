package releaseverify

import (
	"context"
	"io/fs"
)

// VerifySignature authenticates the checksum list with the compiled release key.
// It needs only the checksum list and bundle, before any artifact is downloaded.
func VerifySignature(ctx context.Context, files fs.FS) error {
	key, err := PublicKey()
	if err != nil {
		return Key
	}
	_, _, err = verifiedManifest(ctx, files, key)
	return err
}

func verifiedManifest(ctx context.Context, files fs.FS, key []byte) ([]byte, []byte, error) {
	sums, err := readMetadata(ctx, files, "SHA256SUMS")
	if err != nil {
		return nil, nil, err
	}
	bundle, err := readMetadata(ctx, files, "SHA256SUMS.sigstore.json")
	if err != nil {
		return nil, nil, err
	}
	if err = verifyBlob(sums, bundle, key); err != nil {
		return nil, nil, err
	}
	return sums, bundle, nil
}
