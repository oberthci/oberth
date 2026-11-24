package installer

import (
	"context"
	"io"

	"github.com/oberthci/oberth/internal/secretstore"
)

const StoreSnapshotHint = "Database-only backups exclude store data; keep secure originals to put again."

// SecretPutRequest contains public grant selectors; values are read from a stream.
type SecretPutRequest struct{ Repo, File, Step, Path string }

// SecretPutPort is implemented by the authenticated service client, never a cluster client.
type SecretPutPort interface {
	PutSecret(context.Context, SecretPutRequest, []byte) error
}

func SecretPut(ctx context.Context, port SecretPutPort, request SecretPutRequest, in io.Reader) error {
	if port == nil || in == nil {
		return problem("secret-input", nil)
	}
	value, err := io.ReadAll(io.LimitReader(in, secretstore.MaxValueBytes+1))
	defer clear(value)
	if err != nil || len(value) > secretstore.MaxValueBytes {
		return problem("secret-input", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return port.PutSecret(ctx, request, value)
}
