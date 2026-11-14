package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Named reads never accept a caller-supplied path. The fallback order matches
// the mounted public identities; private material is never in this inventory.
func readAdminPublicIdentity(ctx context.Context, name string) ([]byte, error) {
	var paths []string
	switch name {
	case "server-certificate":
		paths = []string{"/etc/oberth/tls/tls.crt", "/run/oberth-identities/server/tls.crt"}
	case "server-ca":
		paths = []string{"/run/oberth-identities/server/ca.crt", "/run/oberth-identities/server/tls.crt", "/etc/oberth/tls/tls.crt"}
	case "upstream-public-key":
		paths = []string{"/run/oberth-identities/upstream/id_ed25519.pub"}
	default:
		return nil, refuse(ErrInvalidInput, "unknown public identity name")
	}
	for _, path := range paths {
		body, err := readPublicIdentityFile(ctx, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return body, err
	}
	return nil, refuse(ErrUnavailable, "public identity is not materialized")
}

func readPublicIdentityFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(directory, resolved)
	if err != nil || !filepath.IsLocal(relative) || filepath.Base(resolved) != filepath.Base(path) {
		return nil, refuse(ErrInvalidInput, "public identity resolves outside its identity directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entry, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !entry.Mode().IsRegular() {
		return nil, refuse(ErrInvalidInput, "public identity must be a regular file")
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(entry, info) {
		return nil, refuse(ErrInvalidInput, "public identity changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 128<<10 {
		return nil, refuse(ErrInvalidInput, "public identity exceeds 128 KiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}
