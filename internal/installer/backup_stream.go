package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// stderr is diagnostic only, never part of the binary archive. Retain just a
// small prefix, while consuming the rest so an untrusted child cannot block.
type backupDiagnostic struct{ data []byte }

func (b *backupDiagnostic) Write(p []byte) (int, error) {
	if left := 4096 - len(b.data); left > 0 {
		b.data = append(b.data, p[:min(left, len(p))]...)
	}
	return len(p), nil
}

func defaultRunCommandIO(ctx context.Context, input io.Reader, output io.Writer, name string, args ...string) error {
	// #nosec G204 -- fixed executable and structured arguments, no shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout = input, output
	stderr := &backupDiagnostic{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w%s", name, err, commandOutputSuffix(stderr.data))
	}
	return ctx.Err()
}

type backupLimitWriter struct {
	output   io.Writer
	ctx      context.Context
	cancel   context.CancelFunc
	n, limit int64
	err      error
}

func (w *backupLimitWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-w.n {
		w.err = backupLimitError(w.n+int64(len(p)), w.limit)
		w.cancel() // Do not wait for a blocked stdout producer to exit naturally.
		return 0, w.err
	}
	n, err := w.output.Write(p)
	w.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}

// Files live only in a fresh private working directory. A failed/oversized
// transfer removes its partial output; no database-sized byte slice is retained.
func streamBackupFile(ctx context.Context, deps backupDeps, destination string, limit int64, args []string) (digest string, resultErr error) {
	if deps.runIO == nil {
		return "", errors.New("backup stdout stream runner unavailable")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600) // #nosec G304 -- fixed name in private backup working directory.
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(destination))
		}
	}()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	hash := sha256.New()
	writer := &backupLimitWriter{output: io.MultiWriter(file, hash), ctx: child, cancel: cancel, limit: limit}
	err = deps.runIO(child, nil, writer, "kubectl", args...)
	if err := errors.Join(writer.err, err, ctx.Err()); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Local staging also observes cancellation during large copies.
type backupContextReader struct {
	io.Reader
	ctx context.Context
}

func (r *backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
