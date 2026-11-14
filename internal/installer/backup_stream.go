package installer

import (
	"context"
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

func defaultRunCommandIO(ctx context.Context, input io.Reader, output io.Writer, name string, args ...string) (resultErr error) {
	args, environment, lease, cleanup, prepareErr := pinnedKubernetesCommand(name, args, installerChildEnvironment())
	if prepareErr != nil {
		return prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	// #nosec G204 -- fixed executable and structured arguments, no shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environment
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease}
	}
	cmd.Stdin, cmd.Stdout = input, output
	stderr := &backupDiagnostic{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w%s", name, err, commandOutputSuffix(stderr.data))
	}
	return ctx.Err()
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
