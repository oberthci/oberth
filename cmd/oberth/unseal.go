package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func runUnseal(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("unseal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.Config
	flags.StringVar(&cfg.OpenBaoNamespace, "openbao-namespace", "", "OpenBao namespace (default: openbao)")
	timeout := flags.Duration("timeout", time.Minute, "wait timeout for the OpenBao pod to answer bao status")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unseal accepts flags only, no positional arguments", errUsage)
	}

	cfg.Timeout = *timeout
	cfg.BinaryVersion = version

	return installer.Unseal(ctx, cfg, installer.InstallDeps{
		Output: output,
		Input:  input,
	})
}
