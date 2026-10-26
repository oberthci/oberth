package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// These local, read-only commands replace file tools in the scratch server.
// Kubernetes exec authorization and the process UID govern access, as before.
func runAdmin(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: admin read-file <path>... | hash-db [--database path]", errUsage)
	}
	switch arguments[0] {
	case "read-file":
		if len(arguments) < 2 {
			return fmt.Errorf("%w: admin read-file requires at least one absolute path", errUsage)
		}
		var last error
		for _, path := range arguments[1:] {
			file, err := openAdminFile(path)
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
	case "hash-db":
		flags := flag.NewFlagSet("admin hash-db", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		database := flags.String("database", "/data/oberth.sqlite", "SQLite main file (and optional WAL)")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("%w: admin hash-db accepts no positional arguments", errUsage)
		}
		for _, suffix := range []string{"", "-wal"} {
			path := *database + suffix
			file, err := openAdminFile(path)
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, err = io.Copy(hash, adminContextReader{ctx, file})
			if failure := errors.Join(err, file.Close()); failure != nil {
				return failure
			}
			if _, err = fmt.Fprintf(output, "%s  %s\n", hex.EncodeToString(hash.Sum(nil)), path); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown admin command %q", errUsage, arguments[0])
	}
}
func openAdminFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, errors.New("admin file path must be absolute and contain no line breaks")
	}
	// Projected Secret paths may be symlinks; follow them just as cat did.
	info, err := os.Stat(path) // #nosec G703 -- Local read-only admin command intentionally accepts operator-selected absolute paths; the process UID governs access.
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("admin file must be regular")
	}
	file, err := os.Open(path) // #nosec G304 G703 -- Same administrator-selected read-only path; no service endpoint or write operation.
	if err != nil {
		return nil, err
	}
	current, err := file.Stat()
	if err != nil || !current.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("admin file must remain regular"))
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
