package main

import (
	"context"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func runWaitIdle(ctx context.Context, args []string, output io.Writer) error {
	flags, connection, parse := adminCLIFlags("wait-idle", args)
	timeout := flags.Duration("timeout", installer.DefaultUpgradeWaitTimeout, "maximum time to observe the authenticated admin queue")
	mode := flags.String("wait-scope", "all", "runs to wait for: all or critical")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth wait-idle [flags]")
	}
	if len(positional) != 0 || *timeout <= 0 || (*mode != "all" && *mode != "critical") {
		return fmt.Errorf("%w: wait-idle requires positive --timeout and --wait-scope=all|critical", errUsage)
	}
	remote, err := remoteClient(ctx, connection)
	if err != nil {
		return err
	}
	if err := installer.WaitForAdminIdle(ctx, remote, *mode, *timeout, output); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s queue is idle\n", *mode)
	return err
}
