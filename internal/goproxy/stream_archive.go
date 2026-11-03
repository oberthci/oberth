package goproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// moduleZip owns a private temporary file, removed when its consumer finishes.
type moduleZip struct{ *os.File }

func (z *moduleZip) Close() error {
	return errors.Join(z.File.Close(), os.Remove(z.Name()))
}

// storeObject streams once to a private temporary file while hashing. The
// digest-named link becomes visible only after the bounded content is synced.
func (c *artifactCache) storeObject(ctx context.Context, source io.Reader) (artifactObject, error) {
	file, err := os.CreateTemp(filepath.Join(c.root, "objects"), ".artifact-*")
	if err != nil {
		return artifactObject{}, err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	hash := sha256.New()
	const limit = 500 << 20
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(proxyContextReader{ctx, source}, limit+1))
	if err != nil {
		return artifactObject{}, err
	}
	if n > limit {
		return artifactObject{}, errors.New("immutable module object exceeds limit")
	}
	object := artifactObject{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n}
	if err = file.Sync(); err != nil {
		return artifactObject{}, err
	}
	if err = file.Close(); err != nil {
		return artifactObject{}, err
	}
	target := c.objectPath(object.SHA256)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return artifactObject{}, err
	}
	if err = syncArtifactDirectory(filepath.Join(c.root, "objects")); err != nil {
		return artifactObject{}, err
	}
	if err = ctx.Err(); err != nil {
		return artifactObject{}, err
	}
	if err = os.Link(file.Name(), target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return artifactObject{}, err
		}
		existing, err := os.Open(target) //nolint:gosec // Digest-only path under the private cache root.
		if err != nil {
			return artifactObject{}, err
		}
		defer func() { _ = existing.Close() }()
		hash.Reset()
		read, err := io.Copy(hash, io.LimitReader(proxyContextReader{ctx, existing}, n+1))
		if err != nil {
			return artifactObject{}, err
		}
		if read != n || hex.EncodeToString(hash.Sum(nil)) != object.SHA256 {
			return artifactObject{}, errors.New("immutable module cache conflict")
		}
	}
	return object, syncArtifactDirectory(filepath.Dir(target))
}

type proxyLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *proxyLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("module zip exceeds size limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
