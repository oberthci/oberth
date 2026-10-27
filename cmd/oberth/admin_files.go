package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// These local, read-only commands replace file tools in the scratch server.
// Kubernetes exec authorization, a public-path allowlist and the process UID govern access.
func runAdmin(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: admin read-file <public-identity-path> [fallback-path]", errUsage)
	}
	switch arguments[0] {
	case "read-file":
		if len(arguments) < 2 {
			return fmt.Errorf("%w: admin read-file requires at least one absolute path", errUsage)
		}
		return adminReadPublicFiles(ctx, arguments[1:], output, adminPublicFiles)
	default:
		return fmt.Errorf("%w: unknown admin command %q", errUsage, arguments[0])
	}
}

// Only public identity material is exposed through read-file. Test fixtures pass
// their own private roots to adminReadPublicFiles; the command always uses this list.
var adminPublicFiles = map[string]bool{
	"/etc/oberth/tls/tls.crt":                        true,
	"/run/oberth-identities/server/ca.crt":           true,
	"/run/oberth-identities/server/tls.crt":          true,
	"/run/oberth-identities/upstream/id_ed25519.pub": true,
}

func adminReadPublicFiles(ctx context.Context, paths []string, output io.Writer, allowed map[string]bool) error {
	// Check every argument before touching the filesystem, including fallbacks.
	for _, path := range paths {
		if !allowed[path] {
			return errors.New("admin read-file permits only public identity files")
		}
	}
	var last error
	for _, path := range paths {
		file, err := openPublicAdminFile(path)
		if errors.Is(err, os.ErrNotExist) {
			last = err
			continue
		}
		if err != nil {
			return err
		}
		_, err = io.Copy(output, adminContextReader{ctx, file})
		return errors.Join(err, file.Close())
	}
	return last
}

func openPublicAdminFile(path string) (*os.File, error) {
	// Kubernetes projects public files through ..data symlinks. Resolve once,
	// require the same public filename beneath its exact identity directory,
	// then use a traversal-resistant root and refuse a changed final symlink.
	directory := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(directory, resolved)
	if err != nil || !filepath.IsLocal(relative) || filepath.Base(resolved) != filepath.Base(path) {
		return nil, errors.New("admin public file resolves outside its allowed public identity path")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("admin public file must be regular"))
	}
	return file, nil
}

type adminContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r adminContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
