package gitcache

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

// Native pipe descriptors have no exec copy worker waiting for a request that
// only the exited child could answer. Exit and cancellation both close them.
func (p *commandProgress) exchange(ctx context.Context, cmd *exec.Cmd, consume func(io.Writer, io.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd.Stdin, cmd.Stdout = nil, nil
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	closePipes := func() { _ = input.Close(); _ = output.Close() }
	stop := context.AfterFunc(ctx, closePipes)
	defer stop()
	done := make(chan error, 1)
	go func() { err := runOwned(ctx, cmd); closePipes(); done <- err }()
	err = consume(commandWriter{Writer: input, progress: p}, commandReader{Reader: output, progress: p})
	_ = input.Close()
	if err != nil {
		cancel()
	}
	return errors.Join(err, <-done, context.Cause(ctx))
}
