package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var errUsage = errors.New("usage error")

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "oberth:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: expected serve, upstream, uplink, secretstore, or version", errUsage)
	}
	switch arguments[0] {
	case "serve":
		return runServe(ctx, arguments[1:], output)
	case "upstream":
		return runUpstream(ctx, arguments[1:], output)
	case "uplink":
		return runUplink(ctx, arguments[1:], input, output)
	case "secretstore":
		return runSecretStore(ctx, arguments[1:], output)
	case "version":
		if len(arguments) != 1 {
			return fmt.Errorf("%w: version accepts no arguments", errUsage)
		}
		_, err := fmt.Fprintf(output, "oberth %s commit=%s date=%s\n", version, commit, date)
		return err
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, arguments[0])
	}
}
